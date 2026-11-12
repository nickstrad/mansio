/**
 * The typed client: handlers run **in the caller's process**
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
 * - Both kinds run the handler with the recording executor (`sdk/recording.ts`). Reads asked for together
 *   are one query request, and all the reads of one run hold in one snapshot: a run whose earlier reads
 *   changed under it is abandoned and the handler runs again (bounded by `handlerRetries`, with jitter,
 *   within the call's deadline).
 * - `client.query(name, args)` answers what the handler returns.
 * - `client.mutation(name, args)` also buffers the handler's writes, and on return one mutation request
 *   carries the derived preconditions (`assert` ops, first) and the writes. A run that wrote nothing
 *   sends no commit: its reads were one snapshot already. A `ConflictError` on a derived precondition
 *   re-runs the handler the same way, starting from the current state the conflict carried (so the re-run
 *   reads again only what the conflict did not cover), under a
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
 * accept 1–128 of `[A-Za-z0-9._:-]` and replace anything else. The handler's name goes with it as
 * `X-Mansio-Handler`, so a server's event log can name the handler a request belongs to.
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
import {
  type Kind as IrKind,
  MAX_OPS,
  type Op,
  type Request,
} from "../core/intermediate-representation/types.ts";
import type { AnyDefinition, ArgsOf, Functions, Kind, Names, ResultOf } from "./functions.ts";
import { type IdSource, uuidv7 } from "./ids.ts";
import { memoryCache, type ReadCache } from "./cache.ts";
import { type Guard, type Reads, type ReadSet, Recording, Restart } from "./recording.ts";
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
  /** Re-runs of a handler after a derived conflict or a snapshot that moved (default 4, so 5 runs). */
  readonly handlerRetries?: number;
  readonly retry?: RetryPolicy;
  /** Shared windowed retry budget (default: one per client, ratio 0.1, burst 10). */
  readonly budget?: RetryBudget;
  readonly maxReroutes?: number;

  /**
   * The document cache a mutation handler's `get` is answered from (`sdk/cache.ts`): every cached read is
   * asserted at the server before the call answers, so a warm mutation is one round trip and a stale
   * entry costs one more, never a wrong commit. Default: an in-memory cache of 1024 documents per client.
   * `false` turns it off: every `get` then asks the server.
   */
  readonly cache?: ReadCache | false;

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

/** A read's failure, carried out of the handler as a defect so `DbError` stays the handler's channel. */
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

/** Answers as they cross HTTP as JSON: `undefined` → `null`, non-JSON values refused. */
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
  const cache = options.cache === false ? undefined : options.cache ?? memoryCache();
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

  /**
   * One handler run on `recording`, with read failures and local failures as `ClientError`s, and a run the
   * recording abandoned as its `Restart`.
   */
  const runHandler = (definition: AnyDefinition, recording: Recording<Schema>, args: unknown) =>
    settle(definition.invoke(recording, args));

  /** `effect` with the recording's defects (`Escape`, `Restart`) and local failures in the error channel. */
  const settle = <A, E>(effect: Effect.Effect<A, E>) =>
    effect.pipe(
      Effect.catchSomeDefect((defect): Option.Option<Effect.Effect<never, ClientError | Restart>> =>
        defect instanceof Escape
          ? Option.some(Effect.fail(defect.error))
          : defect instanceof Restart
          ? Option.some(Effect.fail(defect))
          : Option.none()
      ),
      Effect.mapError((e): ClientError | Restart =>
        e instanceof Restart || isClientError(e) ? e : localFailure(e)
      ),
    );

  const reads = (call: CallContext): Reads => (ops) =>
    Effect.flatMap(request("query", ops), (req) => send(req, call)).pipe(
      Effect.flatMap((response) =>
        typeof response.seq === "number"
          ? Effect.succeed({ result: response.result, seq: response.seq })
          : Effect.fail(
            new RemoteError({
              status: 502,
              remoteTag: "Defect",
              message: "the server answered a query without a commit sequence (it predates IR v1.1)",
            }),
          )
      ),
      // A failed assert is the recording's to judge; everything else leaves the handler as it is.
      Effect.catchAll((error) =>
        error instanceof RemoteError && error.remoteTag === "ConflictError"
          ? Effect.fail(error)
          : Effect.die(new Escape(error))
      ),
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

  /** Whether the run wrote and its commit (derived asserts + writes) would not fit in one request. */
  const tooManyOps = (recording: Recording<Schema>) => {
    const { ops, asserts } = recording.mutation();
    return ops.length > asserts && ops.length > MAX_OPS;
  };
  const opLimit = (name: string, recording: Recording<Schema>) => {
    const { ops, asserts } = recording.mutation();
    return localFailure(
      new RuleError({
        rule: "ir-op-limit",
        detail: `${name} needs ${ops.length} operations (${asserts} derived asserts + ` +
          `${ops.length - asserts} writes); one IR mutation holds at most ${MAX_OPS}`,
      }),
    );
  };

  /** Runs handler `name` until a run stands: its reads held in one snapshot and its commit, if any, applied. */
  const call = (kind: Kind, name: string, args: unknown, call: CallContext) =>
    Effect.gen(function* () {
      const definition = yield* lookup(kind, name);
      // What the next run starts from: the last run's reads, as its conflict said they now are.
      let seed: ReadSet | undefined;
      for (let run = 0;; run++) {
        const recording = new Recording<Schema>({
          reads: reads(call),
          ids: newId,
          now,
          writable: kind === "mutation",
          seed,
          cache,
        });
        // The run's outcome before any commit: the handler's answer as it will cross the wire, or why
        // there is none (its failure, a run the recording abandoned, a commit too large to send).
        let ran = yield* Effect.either(
          runHandler(definition, recording, args).pipe(
            Effect.flatMap(roundTrip),
            Effect.filterOrFail(() => !tooManyOps(recording), () => opLimit(name, recording)),
          ),
        );
        const { ops, asserts } = recording.mutation();
        const commits = ran._tag === "Right" && ops.length > asserts;
        // A run that ends without a commit answers from its reads alone: any that came from the cache are
        // asserted first, and a stale one re-runs the handler.
        if (!commits && recording.pending && !(ran._tag === "Left" && ran.left instanceof Restart)) {
          const verified = yield* Effect.either(settle(recording.verify()));
          if (verified._tag === "Left") {
            ran = verified;
          }
        }
        let error: ClientError;
        if (ran._tag === "Left") {
          if (!(ran.left instanceof Restart)) {
            return yield* ran.left;
          }
          error = ran.left.error;
          seed = ran.left.seed;
        } else {
          if (!commits) {
            return ran.right; // wrote nothing: its reads were one snapshot already
          }
          // A re-run is a new request: its op list differs (fresh reads, fresh ids), and the last one failed.
          const committed = yield* Effect.either(
            Effect.flatMap(request("mutation", ops, newRequestId()), (req) => send(req, call)),
          );
          if (committed._tag === "Right") {
            recording.committed(committed.right.result.slice(asserts));
            return ran.right;
          }
          error = committed.left;
          recording.forget();
          if (!derivedConflict(error, ops, asserts)) return yield* error;
          seed = recording.rebase(error as RemoteError, ops.slice(0, asserts) as Guard[]);
        }
        // Someone else wrote what this run read: re-run the handler from what the conflict said, after a
        // jittered pause.
        if (run >= handlerRetries) return yield* error;
        const wait = Math.floor(random() * Math.min(250, 10 * 2 ** run));
        if (now() + wait >= call.deadline) return yield* error;
        yield* sleep(wait);
      }
    });

  const context = (name: string, traceId?: string): CallContext => ({
    traceId: traceId ?? newTraceId(),
    deadline: now() + timeoutMs,
    handler: name,
  });

  return {
    query: <N extends Names<F, "query">>(
      name: N,
      args: ArgsOf<F[N]>,
      traceId?: string,
    ): Effect.Effect<ResultOf<F[N]>, ClientError> =>
      Effect.suspend(() => call("query", name, args, context(name, traceId))) as Effect.Effect<
        ResultOf<F[N]>,
        ClientError
      >,
    mutation: <N extends Names<F, "mutation">>(
      name: N,
      args: ArgsOf<F[N]>,
      traceId?: string,
    ): Effect.Effect<ResultOf<F[N]>, ClientError> =>
      Effect.suspend(() => call("mutation", name, args, context(name, traceId))) as Effect.Effect<
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

/** The promise-shaped client, for callers that are not Effect programs. */
export function createPromiseClient<F extends Functions>(options: ClientOptions<F>) {
  const client = createClient(options);
  return {
    query: <N extends Names<F, "query">>(name: N, args: ArgsOf<F[N]>, traceId?: string) =>
      runPromise(client.query(name, args, traceId)),
    mutation: <N extends Names<F, "mutation">>(name: N, args: ArgsOf<F[N]>, traceId?: string) =>
      runPromise(client.mutation(name, args, traceId)),
  };
}
