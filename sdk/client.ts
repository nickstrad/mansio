/**
 * The typed client (IR-3, `docs/plans/ir-protocol.md` §3): handlers run **in the caller's process**
 * against the remote database over `/v1/ops`.
 *
 * ```ts
 * const client = createClient({
 *   functions, schema,                       // values: the client runs the handlers
 *   tenant: "acme", app: "tic-tac-toe",
 *   endpoint: "http://127.0.0.1:7070", token: ticket,       // single node, or:
 *   // locator: { url: "http://controld:7080", apiKey: "mansio_…" },
 * });
 * const { gameId } = yield* client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" });
 * ```
 *
 * - `client.query(name, args)` runs the query handler locally; each of its reads is one query request
 *   (so two reads are two snapshots, in v1).
 * - `client.mutation(name, args)` runs the handler with the recording executor (`sdk/recording.ts`):
 *   reads go out at once, writes are buffered, and on return one mutation request carries the derived
 *   preconditions (`assert` ops, first) and the writes. A `ConflictError` on a derived precondition
 *   re-runs the handler (bounded by `handlerRetries`, with jitter, within the call's deadline), under a
 *   new `requestId` (transport retries of one commit keep theirs). A commit holds at most 64 ops
 *   (derived asserts + writes); more fails locally with `RuleError` `ir-op-limit`. An app-raised
 *   `ConflictError` (e.g. `makeMove`'s `expectedVersion`) fails the handler locally and goes to the
 *   caller; it never reaches the server. **Handlers may run more than once: no side effects beyond
 *   `db`** (no email, no external calls, no clocks or randomness the result depends on).
 * - The answer is JSON round-tripped, as it was when it crossed HTTP.
 *
 * Errors (`ClientError`): `RemoteError` for a definite failure, whether decided by the server (any
 * `/v1/ops` error tag, with `op` and `fields`) or by the handler in this process (`RuleError`,
 * `NotFoundError`, `ConflictError`, a `SchemaError` from argument parsing — reported with the status the
 * server would have used, and the handler's error as `cause`); `Unavailable` when no definite answer came
 * (see `core/errors.ts`). Transport behavior is `sdk/transport.ts`'s.
 *
 * ## Trace ids
 *
 * One `X-Trace-Id` per call: the `traceId` argument verbatim, else `newTraceId()` (default
 * `crypto.randomUUID()`), sent on every request of the call (each read, the commit, every retry). Servers
 * accept 1–128 of `[A-Za-z0-9._:-]` and replace anything else.
 *
 * `createLegacyClient` is the pre-IR client (server-side handlers on the Deno platform's
 * `/apps/:app/:kind/:name`), kept for the tests and tools that exercise that route until IR-6 deletes it.
 */
import { Cause, Effect, Exit, Option } from "effect";
import {
  type ClientError,
  type PlatformError,
  RemoteError,
  RuleError,
  SchemaError,
  toHttpStatus,
  Unavailable,
} from "../core/errors.ts";
import { schemaFingerprint } from "./fingerprint.ts";
import { type Kind as IrKind, MAX_OPS, type Op, type Request } from "../core/ir/types.ts";
import type { AnyDefinition, ArgsOf, Functions, Kind, Names, ResultOf } from "./functions.ts";
import { type IdSource, uuidv7 } from "./ids.ts";
import { type Reads, Recording } from "./recording.ts";
import type { RetryBudget, RetryPolicy } from "./retry.ts";
import { formatSchemaError, type Schema } from "./schema.ts";
import {
  type CallContext,
  type LocatorOptions,
  locatorRouter,
  makeTransport,
  type Send,
  staticRouter,
  type TokenSource,
} from "./transport.ts";

export type { TokenSource } from "./transport.ts";
export { schemaFingerprint } from "./fingerprint.ts";

export interface ClientOptions<F extends Functions> {
  /** The functions object (`defineFunctions(...)` handlers): the client runs them. */
  readonly functions: F;
  /** The schema value, in the database's storage layout (`withStorage(schema, "json")` for a json app). */
  readonly schema: Schema;
  /** The database `(tenant, app)`. On the Deno shim the credentials decide the tenant and this is ignored. */
  readonly tenant: string;
  readonly app: string;

  /** Single node: the base URL `/v1/ops` is served under (agentd `http://host:port`, Deno `…/apps/<app>`). */
  readonly endpoint?: string;
  /** Single node: the bearer, or a function asked per request kind (a ticket minter). */
  readonly token?: TokenSource;
  /** Multi-node: controld's locator and an API key; replaces `endpoint` and `token`. */
  readonly locator?: Omit<LocatorOptions, "tenant" | "app" | "fetch" | "now" | "random">;

  /** One call's deadline, ms, across all its requests and handler runs (default 10 000). */
  readonly timeoutMs?: number;
  /** Re-runs of a mutation handler after a derived conflict (default 4, so 5 runs). */
  readonly handlerRetries?: number;
  readonly retry?: RetryPolicy;
  /** Shared windowed retry budget (default: one per client, ratio 0.1, burst 10). */
  readonly budget?: RetryBudget;
  readonly maxReroutes?: number;

  readonly newTraceId?: () => string;
  /** Document ids for inserts (default UUIDv7). */
  readonly newId?: IdSource;
  /** Mutation request ids (default UUIDv7). */
  readonly newRequestId?: IdSource;

  /** Replaces the HTTP transport (tests, simulations). */
  readonly transport?: Send;
  readonly fetch?: typeof fetch;
  readonly now?: () => number;
  readonly random?: () => number;
  readonly sleep?: (ms: number) => Effect.Effect<void>;
}

/** A read failure, carried out of the handler as a defect so `DbError` stays the handler's channel. */
class Escape {
  constructor(readonly error: ClientError) {}
}

const MESSAGES: Record<string, (e: Record<string, unknown>) => string> = {
  ConflictError: (e) => `Version conflict: expected ${e.expected}, found ${e.actual}`,
  NotFoundError: (e) => `Document not found: ${e.table} ${e.id}`,
  RuleError: (e) => String(e.detail ?? e.rule),
};

/** A failure decided in this process, reported as the server would have reported it. */
export function localFailure(error: unknown): RemoteError {
  const tag = (error as { _tag?: unknown } | null)?._tag;
  if (typeof tag !== "string") {
    return new RemoteError({ status: 500, remoteTag: "Defect", message: String(error), cause: error });
  }
  let status = 500;
  try {
    status = toHttpStatus(error as PlatformError) ?? 500;
  } catch { /* an app-defined tag: a server fault, as the platform treated it */ }
  const fields = error as Record<string, unknown>;
  const message = error instanceof SchemaError ? formatSchemaError(error).message : MESSAGES[tag]?.(fields) ??
    (typeof fields.message === "string" && fields.message ? fields.message : tag);
  return new RemoteError({ status, remoteTag: tag, message, cause: error });
}

const isClientError = (e: unknown): e is ClientError => e instanceof RemoteError || e instanceof Unavailable;

/** Answers as they crossed HTTP before IR-3: `undefined` → `null`, non-JSON values refused. */
function roundTrip(result: unknown): Effect.Effect<unknown, RemoteError> {
  return Effect.try({
    try: () => JSON.parse(JSON.stringify(result ?? null)) as unknown,
    catch: (cause) =>
      new RemoteError({
        status: 500,
        remoteTag: "NonSerializableResultError",
        message: String(cause),
        cause,
      }),
  });
}

/**
 * Is a conflict one the client caused, so that re-running the handler is the fix? Yes when the server
 * blamed a derived assert (they are ops `0 .. asserts-1`), or an insert's client id (`check: "exists"`:
 * a re-run draws new ids). Anything else goes to the caller.
 */
export function derivedConflict(error: ClientError, ops: readonly Op[], asserts: number): boolean {
  if (!(error instanceof RemoteError) || error.remoteTag !== "ConflictError" || error.op === undefined) {
    return false;
  }
  return error.op < asserts || (ops[error.op]?.op === "insert" && error.fields?.check === "exists");
}

export function createClient<F extends Functions>(options: ClientOptions<F>) {
  const now = options.now ?? Date.now;
  const random = options.random ?? Math.random;
  const sleep = options.sleep ?? ((ms: number) => Effect.sleep(ms));
  const newTraceId = options.newTraceId ?? (() => crypto.randomUUID());
  const newId = options.newId ?? uuidv7;
  const newRequestId = options.newRequestId ?? uuidv7;
  const timeoutMs = options.timeoutMs ?? 10_000;
  const handlerRetries = options.handlerRetries ?? 4;
  const db = { tenant: options.tenant, app: options.app };
  if (!options.transport && !options.locator && !options.endpoint) {
    throw new TypeError("createClient: give endpoint (single node), locator (multi-node) or transport");
  }
  let fingerprint: Promise<string> | undefined;
  const schema = Effect.promise(() => fingerprint ??= schemaFingerprint(options.schema));

  const send: Send = options.transport ?? makeTransport({
    router: options.locator
      ? locatorRouter({ ...options.locator, ...db, fetch: options.fetch, now, random })
      : staticRouter(options.endpoint!, options.token ?? ""),
    retry: options.retry,
    budget: options.budget,
    maxReroutes: options.maxReroutes,
    fetch: options.fetch,
    now,
    random,
    sleep,
  });

  const request = (kind: IrKind, ops: Op[], requestId?: string) =>
    Effect.map(schema, (fp): Request => ({
      v: 1,
      db,
      kind,
      ...(requestId === undefined ? {} : { requestId }),
      schema: fp,
      ops,
    }));

  /** One handler run on `recording`, with read failures and local failures as `ClientError`s. */
  const runHandler = (definition: AnyDefinition, recording: Recording<Schema>, args: unknown) =>
    definition.invoke(recording, args).pipe(
      Effect.catchSomeDefect((defect) =>
        defect instanceof Escape ? Option.some(Effect.fail(defect.error)) : Option.none()
      ),
      Effect.mapError((e): ClientError => isClientError(e) ? e : localFailure(e)),
    );

  const reads = (call: CallContext): Reads => (ops) =>
    Effect.flatMap(request("query", ops), (req) => send(req, call)).pipe(
      Effect.map((response) => response.result),
      Effect.catchAll((error) => Effect.die(new Escape(error))),
    );

  const lookup = (kind: Kind, name: string): Effect.Effect<AnyDefinition, RemoteError> => {
    if (!Object.hasOwn(options.functions, name)) {
      return Effect.fail(
        new RemoteError({
          status: 404,
          remoteTag: "UnknownHandlerError",
          message: `Unknown handler: ${name}`,
        }),
      );
    }
    const definition = options.functions[name];
    if (definition.kind !== kind) {
      return Effect.fail(
        new RemoteError({
          status: 400,
          remoteTag: "HandlerKindMismatch",
          message: `Handler kind mismatch: ${name} is a ${definition.kind}, not a ${kind}`,
        }),
      );
    }
    return Effect.succeed(definition);
  };

  const query = (name: string, args: unknown, call: CallContext) =>
    Effect.gen(function* () {
      const definition = yield* lookup("query", name);
      const recording = new Recording<Schema>(reads(call), newId, now, false);
      return yield* roundTrip(yield* runHandler(definition, recording, args));
    });

  const mutation = (name: string, args: unknown, call: CallContext) =>
    Effect.gen(function* () {
      const definition = yield* lookup("mutation", name);
      for (let run = 0;; run++) {
        const recording = new Recording<Schema>(reads(call), newId, now, true);
        const result = yield* roundTrip(yield* runHandler(definition, recording, args));
        const { ops, asserts } = recording.mutation();
        if (ops.length === 0) return result; // read nothing, wrote nothing: nothing to commit
        if (ops.length > MAX_OPS) {
          return yield* localFailure(
            new RuleError({
              rule: "ir-op-limit",
              detail: `${name} needs ${ops.length} operations (${asserts} derived asserts + ` +
                `${ops.length - asserts} writes); one IR mutation holds at most ${MAX_OPS}`,
            }),
          );
        }
        // A re-run is a new request: its op list differs (fresh reads, fresh ids), and the last one failed.
        const committed = yield* Effect.either(
          Effect.flatMap(request("mutation", ops, newRequestId()), (req) => send(req, call)),
        );
        if (committed._tag === "Right") return result;
        const error = committed.left;
        if (!derivedConflict(error, ops, asserts) || run >= handlerRetries) return yield* error;
        // Someone else wrote what this run read: re-run the handler on fresh reads, after a jittered pause.
        const wait = Math.floor(random() * Math.min(250, 10 * 2 ** run));
        if (now() + wait >= call.deadline) return yield* error;
        yield* sleep(wait);
      }
    });

  const context = (traceId?: string): CallContext => ({
    traceId: traceId ?? newTraceId(),
    deadline: now() + timeoutMs,
  });

  return {
    query: <N extends Names<F, "query">>(
      name: N,
      args: ArgsOf<F[N]>,
      traceId?: string,
    ): Effect.Effect<ResultOf<F[N]>, ClientError> =>
      Effect.suspend(() => query(name, args, context(traceId))) as Effect.Effect<ResultOf<F[N]>, ClientError>,
    mutation: <N extends Names<F, "mutation">>(
      name: N,
      args: ArgsOf<F[N]>,
      traceId?: string,
    ): Effect.Effect<ResultOf<F[N]>, ClientError> =>
      Effect.suspend(() => mutation(name, args, context(traceId))) as Effect.Effect<
        ResultOf<F[N]>,
        ClientError
      >,
  };
}

/** Runs an effect as a promise that rejects with the `ClientError` itself. */
async function runPromise<A>(effect: Effect.Effect<A, ClientError>): Promise<A> {
  const exit = await Effect.runPromiseExit(effect);
  if (Exit.isSuccess(exit)) return exit.value;
  const failure = Cause.squash(exit.cause);
  throw failure instanceof Error ? failure : new Error(String(failure));
}

/** The promise-shaped client, for callers that are not Effect programs (`scripts/demo.ts`). */
export function createPromiseClient<F extends Functions>(options: ClientOptions<F>) {
  const client = createClient(options);
  return {
    query: <N extends Names<F, "query">>(name: N, args: ArgsOf<F[N]>, traceId?: string) =>
      runPromise(client.query(name, args, traceId)),
    mutation: <N extends Names<F, "mutation">>(name: N, args: ArgsOf<F[N]>, traceId?: string) =>
      runPromise(client.mutation(name, args, traceId)),
  };
}

// ===========================================================================
// The pre-IR client (server-side handlers). Removed with the Deno platform (IR-6).
// ===========================================================================

/** Reads whatever the server put in `error`: a bare string, or a tagged object. */
function remoteFailure(status: number, body: unknown): RemoteError {
  const error = (body as { error?: unknown } | null)?.error;
  if (typeof error === "string" && error) return new RemoteError({ status, message: error });
  if (error && typeof error === "object") {
    const { tag, message } = error as { tag?: unknown; message?: unknown };
    return new RemoteError({
      status,
      remoteTag: typeof tag === "string" ? tag : undefined,
      message: typeof message === "string" && message ? message : `HTTP ${status}`,
    });
  }
  return new RemoteError({ status, message: `HTTP ${status}` });
}

/**
 * @deprecated The pre-IR client: `POST {baseURL}/apps/{appId}/{kind}/{name}`, the handler runs on the
 * Deno server. `token` is the tenant API key in tenancy modes, or the app token with TENANCY=off. A
 * transport failure is a defect, as before IR-3. Kept for the tests of that route (trace, integration,
 * the e2e scenarios IR-4 ports) until IR-6.
 */
export function createLegacyClient<F extends Functions>(
  baseURL: string,
  appId: string,
  token: string,
  newTraceId: () => string = () => crypto.randomUUID(),
) {
  function call<K extends Kind, N extends Names<F, K>>(
    kind: K,
    name: N,
    args: ArgsOf<F[N]>,
    traceId?: string,
  ): Effect.Effect<ResultOf<F[N]>, RemoteError> {
    const url = `${baseURL}/apps/${encodeURIComponent(appId)}/${kind}/${encodeURIComponent(name)}`;
    return Effect.gen(function* () {
      const response = yield* Effect.promise(() =>
        fetch(url, {
          method: "POST",
          headers: {
            "content-type": "application/json",
            authorization: `Bearer ${token}`,
            "x-trace-id": traceId ?? newTraceId(),
          },
          body: JSON.stringify(args),
          signal: AbortSignal.timeout(30_000),
        })
      );
      const body = yield* Effect.promise(() => response.json() as Promise<{ result?: unknown }>);
      if (!response.ok) return yield* remoteFailure(response.status, body);
      return body.result as ResultOf<F[N]>;
    });
  }
  return {
    query: <N extends Names<F, "query">>(name: N, args: ArgsOf<F[N]>, traceId?: string) =>
      call("query", name, args, traceId),
    mutation: <N extends Names<F, "mutation">>(name: N, args: ArgsOf<F[N]>, traceId?: string) =>
      call("mutation", name, args, traceId),
  };
}
