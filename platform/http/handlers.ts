/**
 * The endpoint handlers (plan item 22, work 2): one Effect per route, each returning the value that
 * becomes `{ result }` or failing with a `PlatformError` that `router.ts` turns into a status via
 * `toHttpStatus`.
 *
 * Handlers see a `RequestContext`, not a `Request`: the headers, and a body Effect that reads and
 * parses the JSON body under the configured cap only when a handler asks for it. Every handler
 * authenticates before it reads the body, so an unauthenticated caller cannot make the server read
 * 64 KiB. The checks run in the pre-port order, so each failure keeps its status:
 *
 * ```text
 * POST /apps/:appId/:kind/:name   app exists (404) → app token (401) → handler exists (404)
 *                                 → kind matches (400) → body (413/400) → mutation | query
 * POST /admin/backup/:appId       admin token (401) → app exists (404) → Backups.take
 * POST /admin/logs/ingest         admin token (401) → reader: EventLogIngest
 * POST /admin/logs/analysis/:name admin token (401) → params body (413/400) → reader: Analysis
 * GET  /health                    reader ping → { ok, apps }
 * ```
 *
 * Mutations run on the app's own `Storage` (the SQLite writer, in this process). Queries, ingest and
 * analysis go to the reader process through `ReaderClient`, which owns DuckDB — this process never
 * loads it (ADR 0001). The analysis route takes a catalog name and scalar parameters only: no SQL
 * crosses the HTTP boundary, consistent with the repo's "never raw SQL from callers" posture; the
 * reader resolves the name against its catalog and answers `UnknownAnalysisQuery` (404) on a miss.
 *
 * ## Trace context (item 34)
 *
 * The router runs each request under its trace id (`platform/http/trace.ts`). Every handler here runs
 * its *whole* dispatched Effect — lookup, authentication, body, and the mutation, query or admin
 * call — as one child span (`withChildSpan`), so everything it causes carries the trace and the
 * handler's `spanId`: the SQLite writer's `sql`/`tx` events (on commit and on rollback), backup
 * phases, `Effect.log*` records, and the reader request, whose frame `platform/ipc.ts` stamps with
 * this span as `parentSpanId`.
 *
 * `invoke` (an app handler) also records one `handler` event when it ends, under the same trace and
 * span: `handlerKind`, `name`, `outcome`, `durationMs`, and on a failure the error columns —
 * `errorTag` is the typed failure's tag, `Defect` for a defect, `Interrupted` for an interruption.
 * Platform routes record only the router's `http` event; they have no app handler to describe.
 */
import { Cause, Clock, Effect, Exit, FiberId, Option } from "effect";
import {
  AuthError,
  type HandlerKind,
  HandlerKindMismatch,
  type PlatformError,
  type RequestTooLarge,
  SchemaError,
  toHttpStatus,
  UnknownHandlerError,
} from "../../core/errors.ts";
import { fromPlatformError, handlerEvent, type Level } from "../../core/events.ts";
import { AppServices } from "../registry.ts";
import { AppRegistry, Config, EventSink, ReaderClient, TenantRegistry } from "../services.ts";
import { authenticateAdmin, withAppAuthentication } from "./auth.ts";
import { TenantContext } from "../tenants/routing.ts";
import { parseApiKeyId, parseTenantId } from "../tenants/registry.ts";
import { currentTraceContext, newSpanId, withChildSpan } from "./trace.ts";
import { decodeBody } from "../../core/ir/decode.ts";
import { IrFailure, toWireError } from "../../core/ir/errors.ts";

/** What a handler may read from the request. */
export interface RequestContext {
  readonly headers: Headers;
  /**
   * The JSON body, read under `Config.server.maxRequestBytes` (`RequestTooLarge` → 413) and parsed
   * (`SchemaError` → 400). An absent or empty body is `{}`. Reads the stream once; run it once.
   */
  readonly body: Effect.Effect<unknown, RequestTooLarge | SchemaError>;
  /** The raw body bytes under the route's cap (the ops route decodes these itself). Run it once. */
  readonly bytes: Effect.Effect<Uint8Array, RequestTooLarge>;
}

/**
 * A handler result that is the whole response: `status` and `body` as given, not `{ result }`. The
 * ops route answers with the IR wire form (`ir/spec/v1.md` §8), which carries `op`, `fields` and
 * `duplicate`; `tag` names a failure for the `http` event.
 */
export class RawReply {
  constructor(
    readonly status: number,
    readonly body: unknown,
    readonly tag?: string,
    readonly headers?: Readonly<Record<string, string>>,
  ) {}
}

/** Every service a handler may use. */
export type HandlerRequirements =
  | Config
  | AppRegistry
  | AppServices
  | ReaderClient
  | EventSink
  | TenantRegistry;

export type Handler = Effect.Effect<unknown, PlatformError, HandlerRequirements>;

/** Runs `handler` as a fresh child span of the request (see the module comment). */
function inSpan(handler: Handler): Handler {
  return Effect.flatMap(newSpanId, (spanId) => withChildSpan(spanId, handler));
}

/** What the `handler` event says about an exit: outcome, level, and error columns. */
function handlerOutcome(exit: Exit.Exit<unknown, PlatformError>) {
  if (Exit.isSuccess(exit)) return { outcome: "ok" as const, level: "info" as Level };
  const failure = Cause.failureOption(exit.cause);
  if (Option.isSome(failure)) {
    return {
      outcome: "error" as const,
      level: (toHttpStatus(failure.value) >= 500 ? "error" : "warn") as Level,
      ...fromPlatformError(failure.value),
    };
  }
  const errorTag = Cause.isInterruptedOnly(exit.cause) ? "Interrupted" : "Defect";
  return {
    outcome: "error" as const,
    level: (errorTag === "Defect" ? "error" : "warn") as Level,
    errorTag,
    errorDetail: { message: Cause.pretty(exit.cause).split("\n")[0] },
  };
}

/**
 * `handler` as an app handler's span: it runs inside a fresh child span, and records one `handler`
 * event in that span when it ends — successfully, with a typed failure, a defect or an interruption.
 */
function appHandlerSpan(handlerKind: HandlerKind, name: string, appId: string, handler: Handler): Handler {
  return inSpan(Effect.gen(function* () {
    const started = yield* Clock.currentTimeNanos;
    return yield* Effect.onExit(handler, (exit) =>
      Effect.gen(function* () {
        const finished = yield* Clock.currentTimeNanos;
        const context = Option.getOrUndefined(yield* currentTraceContext);
        const { level, ...columns } = handlerOutcome(exit);
        yield* (yield* EventSink).emit(handlerEvent({
          eventId: crypto.randomUUID(),
          ts: new Date(Number(finished / 1_000_000n)).toISOString(),
          level,
          role: "server",
          pid: Deno.pid,
          // Outside a router-traced request (a direct call in a test), the fiber names the trace,
          // as the SQLite writer's events do.
          traceId: context?.traceId ?? FiberId.threadName(yield* Effect.fiberId),
          ...(context?.spanId === undefined ? {} : { spanId: context.spanId }),
          ...(context?.parentSpanId === undefined ? {} : { parentSpanId: context.parentSpanId }),
          appId,
          handlerKind,
          name,
          durationMs: Number(finished - started) / 1e6,
          ...columns,
        }));
      }));
  }));
}

/** `GET /health`: the reader answers a ping, and the apps this process serves. */
export const health: Handler = inSpan(Effect.gen(function* () {
  const reader = yield* ReaderClient;
  yield* reader.call({ kind: "ping" });
  const apps = yield* (yield* AppRegistry).all;
  return { ok: true, apps: apps.map((app) => app.id) };
}));

/** `POST /apps/:appId/:kind/:name`: run one registered handler as a mutation or a query. */
export function invoke(
  context: RequestContext,
  appId: string,
  kind: HandlerKind,
  name: string,
): Handler {
  return appHandlerSpan(
    kind,
    name,
    appId,
    Effect.gen(function* () {
      const stack = yield* (yield* AppServices).get(appId);
      return yield* withAppAuthentication(
        context.headers,
        stack.app,
        Effect.gen(function* () {
          if (!Object.hasOwn(stack.app.functions, name)) {
            return yield* new UnknownHandlerError({ appId, name });
          }
          const definition = stack.app.functions[name];
          if (definition.kind !== kind) {
            return yield* new HandlerKindMismatch({ appId, name, expected: kind, actual: definition.kind });
          }
          return yield* context.body;
        }),
        (args) =>
          Effect.gen(function* () {
            const definition = stack.app.functions[name];
            if (kind === "mutation") return yield* stack.storage.mutation(definition, args);
            const config = yield* Config;
            if (config.tenancy.mode === "off") {
              return yield* (yield* ReaderClient).call({ kind: "query", appId, name, args });
            }
            const tenant = yield* Effect.serviceOption(TenantContext);
            if (Option.isNone(tenant) || !stack.tenantRouter) {
              return yield* new AuthError({ scope: "app", appId, reason: "missing" });
            }
            const location = yield* stack.tenantRouter.resolve(tenant.value.tenantId);
            yield* stack.tenantRouter.prepare(location);
            return yield* (yield* ReaderClient).call({
              kind: "query",
              appId,
              name,
              args,
              tenant: { id: tenant.value.tenantId, file: location.path },
            });
          }),
      );
    }),
  );
}

/** `POST /admin/backup/:appId`: one backup now. `BackupAlreadyRunning` (409) if one is in flight. */
export function backup(context: RequestContext, appId: string): Handler {
  return inSpan(Effect.gen(function* () {
    yield* authenticateAdmin(context.headers, yield* Config);
    const stack = yield* (yield* AppServices).get(appId);
    return yield* stack.backups.take(appId);
  }));
}

/** `POST /admin/logs/ingest`: the reader ingests new JSONL lines into the event-log DuckDB file. */
export function ingestLogs(context: RequestContext): Handler {
  return inSpan(Effect.gen(function* () {
    yield* authenticateAdmin(context.headers, yield* Config);
    return yield* (yield* ReaderClient).call({ kind: "ingest" });
  }));
}

/** A parameter value an analysis query may bind. Objects and arrays are refused. */
type Scalar = string | number | boolean | null;

/**
 * The analysis body: a flat object of scalars, the catalog query's bind parameters. Anything else is
 * a `SchemaError`, so no structured value (and certainly no SQL fragment the reader might be tempted
 * to splice) reaches the reader.
 */
export function analysisParams(body: unknown): Effect.Effect<Readonly<Record<string, Scalar>>, SchemaError> {
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    return Effect.fail(new SchemaError({ reason: "not-an-object", path: "params", expected: "object" }));
  }
  const params: Record<string, Scalar> = {};
  for (const [key, value] of Object.entries(body)) {
    if (value !== null && !["string", "number", "boolean"].includes(typeof value)) {
      return Effect.fail(
        new SchemaError({
          reason: "type-mismatch",
          path: `params.${key}`,
          expected: "string, number, boolean or null",
        }),
      );
    }
    params[key] = value as Scalar;
  }
  return Effect.succeed(params);
}

/** `POST /admin/logs/analysis/:name`: run one named catalog query with the body as its parameters. */
export function analysis(context: RequestContext, name: string): Handler {
  return inSpan(Effect.gen(function* () {
    yield* authenticateAdmin(context.headers, yield* Config);
    const params = yield* analysisParams(yield* context.body);
    return yield* (yield* ReaderClient).call({ kind: "analysis", name, params });
  }));
}

/** Admin tenant CRUD. Authenticate before parsing identifiers or reading the body. */
export function tenants(
  context: RequestContext,
  method: string,
  rawId?: string,
  keys = false,
  rawKeyId?: string,
): Handler {
  return inSpan(Effect.gen(function* () {
    yield* authenticateAdmin(context.headers, yield* Config);
    const registry = yield* TenantRegistry;
    if (rawId === undefined) {
      if (method === "GET") return yield* registry.listTenants();
      const body = yield* tenantBody(yield* context.body, ["name", "slug"]);
      const name = yield* requiredText(body, "name");
      const slug = yield* requiredText(body, "slug");
      const tenant = yield* registry.createTenant({ name, slug });
      const apiKey = yield* registry.issueApiKey(tenant.id, "default");
      return { tenant, apiKey };
    }
    const id = yield* parseTenantId(rawId);
    if (keys) {
      // Check the URL's tenant even for idempotent revocation of an unknown key.
      yield* registry.getTenant(id);
      if (method === "DELETE") {
        const keyId = yield* parseApiKeyId(rawKeyId!);
        yield* registry.revokeApiKey(keyId, id);
        return { revoked: true };
      }
      const body = yield* tenantBody(yield* context.body, ["name"]);
      return yield* registry.issueApiKey(id, yield* requiredText(body, "name"));
    }
    if (method === "GET") return yield* registry.getTenant(id);
    if (method === "DELETE") return yield* registry.deactivateTenant(id);
    const body = yield* tenantBody(yield* context.body, ["name", "status"]);
    const name = body.name === undefined ? undefined : yield* requiredText(body, "name");
    const status = body.status;
    if (status !== undefined && status !== "active" && status !== "deactivated") {
      return yield* new SchemaError({
        reason: "type-mismatch",
        path: "status",
        expected: "active or deactivated",
      });
    }
    return yield* registry.updateTenant(id, { name, status });
  }));
}

function tenantBody(
  body: unknown,
  fields: readonly string[],
): Effect.Effect<Record<string, unknown>, SchemaError> {
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    return Effect.fail(new SchemaError({ reason: "not-an-object", path: "body", expected: "object" }));
  }
  for (const key of Object.keys(body)) {
    if (!fields.includes(key)) return Effect.fail(new SchemaError({ reason: "unknown-field", path: key }));
  }
  return Effect.succeed(body as Record<string, unknown>);
}

function requiredText(body: Record<string, unknown>, field: string): Effect.Effect<string, SchemaError> {
  const value = body[field];
  return typeof value === "string" && value.trim().length > 0 && !value.includes("\0")
    ? Effect.succeed(value)
    : Effect.fail(
      new SchemaError({ reason: "type-mismatch", path: field, expected: "nonempty string without NUL" }),
    );
}

/** The longest `X-Timeout-Ms` accepted (mansio-server: 1 h). */
const MAX_TIMEOUT_MS = 3_600_000;

/**
 * `POST /apps/:appId/v1/ops`: one IR request (`ir/spec/v1.md`) on the app's own database — the Deno shim
 * of IR-3, so the SDK runs against this backend as against agentd's `/v1/ops`.
 *
 * Auth is the app route's, before the body is read: an app token with TENANCY=off, a tenant API key or
 * session otherwise. The credentials decide the database, as on the handler routes: `db.app` must name
 * this route's app (else `SchemaError` at `db.app`), and `db.tenant` is not used — with TENANCY=off the
 * app has one file, with tenancy the key's tenant file is the database. Then the body is decoded strictly
 * (size, UTF-8, JSON, envelope, operations: `core/ir/decode.ts`) and run by `Storage.ir` on the writer.
 * `X-Timeout-Ms` (1..3 600 000) bounds the wait and the run; past it the answer is `expired` (504), with
 * `fields.ambiguous: true` for a mutation: the deadline may land after COMMIT, so the client retries with
 * the same `requestId` (a commit that landed answers `duplicate`). Queries share the writer queue with
 * mutations (they do not use the reader process), so heavy writes can make them `shed` (503,
 * `Retry-After: 1`) too; that is the shim's limit until IR-6 removes it.
 */
export function ops(context: RequestContext, appId: string): Handler {
  const reply = (failure: IrFailure) => {
    const wire = toWireError(failure);
    return new RawReply(wire.status, wire.body, wire.body.error.tag);
  };
  const mansio = (status: number, tag: string, message: string, headers?: Record<string, string>) =>
    new RawReply(status, { error: { tag, message } }, tag, headers);
  return inSpan(Effect.gen(function* () {
    const stack = yield* (yield* AppServices).get(appId);
    const header = context.headers.get("x-timeout-ms");
    const timeoutMs = header === null ? undefined : Number(header);
    if (
      header !== null &&
      (!/^\d+$/.test(header) || !Number.isSafeInteger(timeoutMs) || timeoutMs! < 1 ||
        timeoutMs! > MAX_TIMEOUT_MS)
    ) {
      return reply(
        new IrFailure({
          cause: new SchemaError({
            reason: "type-mismatch",
            path: "header:X-Timeout-Ms",
            expected: `an integer 1..${MAX_TIMEOUT_MS}`,
          }),
        }),
      );
    }
    return yield* withAppAuthentication(
      context.headers,
      stack.app,
      context.bytes,
      (bytes) =>
        Effect.gen(function* () {
          const request = yield* decodeBody(bytes as Uint8Array);
          if (request.db.app !== appId) {
            return yield* new IrFailure({
              cause: new SchemaError({
                reason: "type-mismatch",
                path: "db.app",
                expected: appId,
                actual: request.db.app,
              }),
            });
          }
          const run = stack.storage.ir(request);
          if (timeoutMs === undefined) return new RawReply(200, yield* run);
          const answered = yield* Effect.timeoutOption(run, timeoutMs);
          if (answered._tag === "Some") return new RawReply(200, answered.value);
          // The deadline can land after COMMIT and before the answer: a mutation may have applied.
          return new RawReply(
            504,
            {
              error: {
                tag: "expired",
                message: "the request's deadline passed",
                ...(request.kind === "mutation" ? { fields: { ambiguous: true } } : {}),
              },
            },
            "expired",
          );
        }),
    ).pipe(
      Effect.catchTags({
        IrFailure: (failure) => Effect.succeed(reply(failure)),
        RequestTooLarge: (cause) => Effect.succeed(reply(new IrFailure({ cause }))),
        QueueFullError: () =>
          Effect.succeed(mansio(503, "shed", "the writer queue is full", { "retry-after": "1" })),
      }),
    );
  }));
}
