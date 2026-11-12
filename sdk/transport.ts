/**
 * The `/v1/ops` transport: sends one IR request and answers its decoded response, with the
 * client behavior of mansio-server's `internal/client`, so the SDK and the Go client fail and retry
 * the same way.
 *
 * Per request:
 *
 * - **Encoding.** The body is `core/ir/json.ts`'s canonical JSON, built once: every attempt sends the same
 *   bytes, so a retried mutation keeps its `requestId` and a commit that already applied answers
 *   `duplicate: true` instead of applying twice (spec §7).
 * - **Headers.** `Authorization: Bearer <ticket | token>`, `X-Trace-Id` (one per call, the same on every
 *   attempt and every request of a handler run), `X-Timeout-Ms` a little below the time left, and
 *   `X-Mansio-Handler` (the handler's name, when the call has one: informational, the server's event log
 *   records it as the request's handler; agentd ignores an invalid one).
 * - **One retry layer.** `shed`, `unavailable`, `aborted` and `expired` retry after
 *   `max(Retry-After, jittered backoff)` while the policy, the shared windowed budget and the deadline
 *   allow. `wrong_owner`/`fenced` (re-resolve with `min_epoch = epoch + 1`), `ticket_invalid` (a fresh
 *   ticket) and no answer reroute through the `Router`; reroutes are bounded (5) and do not spend the
 *   budget (they redirect load rather than repeat it).
 * - **Errors.** A definite answer (any IR tag, or a final mansio code) is a `RemoteError` with the status,
 *   tag, `op` and `fields`; running out of retries, reroutes or time is `Unavailable`, with
 *   `mayHaveApplied` for a mutation that may have committed (no answer, an ambiguous `expired`).
 *
 * Two routers: `staticRouter` (one endpoint, a fixed token or a token function: single-node agentd with
 * a minted ticket, or the Deno shim with an app token / API key) and `locatorRouter` (controld's
 * `POST /v1/resolve` with an API key, a cached route per database refreshed ahead of its ticket's TTL,
 * single-flight).
 */
import { Effect } from "effect";
import { RemoteError, Unavailable } from "../core/errors.ts";
import { canonical } from "../core/ir/json.ts";
import type { Kind, Request, Response } from "../core/ir/types.ts";
import {
  backoff,
  decide,
  DEFAULT_MAX_REROUTES,
  DEFAULT_RETRY,
  mayHaveApplied as codeMayHaveApplied,
  minEpochAfter,
  refreshAt,
  resolveStep,
  retryAfterMs,
  RetryBudget,
  type RetryPolicy,
  timeoutHeader,
  TRANSPORT,
  usableUntil,
} from "./retry.ts";

/** Where one attempt goes. */
export interface Target {
  /** The full `/v1/ops` URL. */
  readonly url: string;
  readonly token: string;
  /** The placement epoch the route was resolved at (locator routes only). */
  readonly epoch?: number;
}

/** A locator (or token source) failure: a resolve answer other than `ok`, or none. */
export class RouteFailure {
  constructor(
    readonly code: string,
    readonly status: number,
    readonly message: string,
    readonly retryAfterMs = 0,
  ) {}
}

export interface Router {
  /** The target for a request of `kind`. */
  route(kind: Kind): Promise<Target>;
  /** After `code` from `target`: drop what is stale. False = nothing to try instead. */
  reroute(target: Target, code: string): boolean;
}

export interface TransportOptions {
  readonly router: Router;
  readonly retry?: RetryPolicy;
  readonly budget?: RetryBudget;
  readonly maxReroutes?: number;
  readonly fetch?: typeof fetch;
  readonly now?: () => number;
  readonly random?: () => number;
  readonly sleep?: (ms: number) => Effect.Effect<void>;
}

/** Per call: the trace id, the absolute deadline (ms, on `now`'s clock) and the handler's name, if any. */
export interface CallContext {
  readonly traceId: string;
  readonly deadline: number;
  readonly handler?: string;
}

export type Send = (
  request: Request,
  call: CallContext,
) => Effect.Effect<Response, RemoteError | Unavailable>;

/** One attempt's answer, as the loop sees it. */
interface Answer {
  readonly code: string; // "ok", a wire tag, or TRANSPORT
  readonly status: number;
  readonly body?: Record<string, unknown>;
  readonly retryAfterMs: number;
  readonly message: string;
}

function answerOf(status: number, body: unknown, header: string | null): Answer {
  const record = body !== null && typeof body === "object" ? body as Record<string, unknown> : undefined;
  if (status === 200 && record && Array.isArray(record.result)) {
    return { code: "ok", status, body: record, retryAfterMs: 0, message: "ok" };
  }
  const error = record?.error as { tag?: unknown; message?: unknown } | undefined;
  if (!error || typeof error.tag !== "string") {
    return {
      code: TRANSPORT,
      status,
      retryAfterMs: 0,
      message: `HTTP ${status} answer without an error tag`,
    };
  }
  return {
    code: error.tag,
    status,
    body: record,
    retryAfterMs: retryAfterMs(header),
    message: typeof error.message === "string" ? error.message : error.tag,
  };
}

function remoteErrorOf(answer: Answer): RemoteError {
  const error = (answer.body?.error ?? {}) as { op?: unknown; fields?: unknown };
  return new RemoteError({
    status: answer.status,
    remoteTag: answer.code,
    message: answer.message,
    ...(typeof error.op === "number" ? { op: error.op } : {}),
    ...(error.fields && typeof error.fields === "object"
      ? { fields: error.fields as Record<string, unknown> }
      : {}),
  });
}

/** The sender for one client. Its budget is shared by every call (and may be shared across clients). */
export function makeTransport(options: TransportOptions): Send {
  const policy = options.retry ?? DEFAULT_RETRY;
  const budget = options.budget ?? new RetryBudget();
  const maxReroutes = options.maxReroutes ?? DEFAULT_MAX_REROUTES;
  const fetchFn = options.fetch ?? globalThis.fetch;
  const now = options.now ?? Date.now;
  const random = options.random ?? Math.random;
  const sleep = options.sleep ?? ((ms: number) => Effect.sleep(ms));

  const attempt = (target: Target, body: string, call: CallContext): Effect.Effect<Answer> =>
    Effect.promise(async () => {
      const remaining = call.deadline - now();
      try {
        const response = await fetchFn(target.url, {
          method: "POST",
          headers: {
            "content-type": "application/json",
            authorization: `Bearer ${target.token}`,
            "x-trace-id": call.traceId,
            "x-timeout-ms": String(timeoutHeader(remaining)),
            ...(call.handler === undefined ? {} : { "x-mansio-handler": call.handler }),
          },
          body,
          signal: AbortSignal.timeout(Math.max(1, remaining)),
        });
        const text = await response.text();
        let parsed: unknown;
        try {
          parsed = JSON.parse(text);
        } catch {
          return { code: TRANSPORT, status: response.status, retryAfterMs: 0, message: "unreadable answer" };
        }
        return answerOf(response.status, parsed, response.headers.get("retry-after"));
      } catch (cause) {
        return { code: TRANSPORT, status: 0, retryAfterMs: 0, message: String(cause) };
      }
    });

  return (request, call) =>
    Effect.gen(function* () {
      const body = canonical(request);
      const mutation = request.kind === "mutation";
      budget.first();
      let reroutes = 0, retries = 0;
      let mayHaveApplied = false;
      let last: Answer | undefined;
      const unavailable = (code: string, message: string, status?: number) =>
        new Unavailable({ code, message, mayHaveApplied, ...(status ? { status } : {}) });
      for (;;) {
        const routed = yield* Effect.either(Effect.tryPromise({
          try: () => options.router.route(request.kind),
          catch: (cause) =>
            cause instanceof RouteFailure ? cause : new RouteFailure(TRANSPORT, 0, String(cause)),
        }));
        let step: "retry" | "reroute";
        let target: Target | undefined;
        let hint = 0;
        if (routed._tag === "Left") {
          const failure = routed.left;
          const code = `resolve:${failure.code}`;
          hint = failure.retryAfterMs;
          const next = resolveStep(failure.code);
          if (next === "reroute" || next === "retry") step = next;
          else {
            return yield* new RemoteError({
              status: failure.status,
              remoteTag: code,
              message: `resolve: ${failure.message}`,
            });
          }
          last = { code, status: failure.status, retryAfterMs: hint, message: failure.message };
        } else {
          target = routed.right;
          // Never send with no time left: nothing has been sent for this attempt, so it cannot have applied.
          if (now() >= call.deadline) {
            return yield* unavailable(
              "deadline",
              `the call's deadline passed before sending; last ${last?.code ?? "none"}`,
            );
          }
          const answer = yield* attempt(target, body, call);
          last = answer;
          hint = answer.retryAfterMs;
          if (mutation && codeMayHaveApplied(answer.code)) mayHaveApplied = true;
          const next = decide(answer.code);
          if (next === "done") return answer.body as unknown as Response;
          // expired is final like any other failure, but it is the server not answering in time, not an
          // application error: the caller gets Unavailable (with mayHaveApplied), as when retries run out.
          if (next === "fail" && answer.code === "expired") {
            return yield* unavailable(answer.code, answer.message, answer.status);
          }
          if (next === "fail") return yield* remoteErrorOf(answer);
          if (now() >= call.deadline) {
            return yield* unavailable("deadline", `the call's deadline passed; last answer ${answer.code}`);
          }
          step = next;
        }

        let wait: number;
        if (step === "reroute") {
          const code = last.code;
          if (target && !options.router.reroute(target, code)) {
            return code === "ticket_invalid"
              ? yield* remoteErrorOf(last)
              : yield* unavailable(code, last.message, last.status || undefined);
          }
          // The first reroute of a call is free; each later one takes a token of the retry budget, unless it
          // is controld's "moving" (no target: the resolve itself said so), which paces a planned move.
          if (reroutes >= maxReroutes || (reroutes >= 1 && target !== undefined && !budget.tryRetry())) {
            return yield* unavailable(code, last.message, last.status || undefined);
          }
          reroutes++;
          wait = backoff(policy, reroutes, hint, random());
        } else {
          if (retries + 1 >= policy.maxAttempts || !budget.tryRetry()) {
            return yield* unavailable(last.code, last.message, last.status || undefined);
          }
          retries++;
          wait = backoff(policy, retries, hint, random());
        }
        if (now() + wait >= call.deadline) {
          return yield* unavailable(
            last.code,
            `${last.message} (no time left to retry)`,
            last.status || undefined,
          );
        }
        yield* sleep(wait);
      }
    });
}

// ===========================================================================
// Routers
// ===========================================================================

/** The token for a request: a fixed string, or a function (e.g. a ticket minter) asked per kind. */
export type TokenSource = string | ((kind: Kind) => string | Promise<string>);

/**
 * One endpoint, one token source. `endpoint` is the base URL the server serves `/v1/ops` under
 * (agentd: `http://host:port`; the Deno shim: `http://host:port/apps/<app>`). A function token is
 * cached per kind and asked again after `ticket_invalid` (once per request, through `reroute`).
 */
export function staticRouter(endpoint: string, token: TokenSource): Router {
  const url = `${endpoint.replace(/\/+$/, "")}/v1/ops`;
  const cached = new Map<Kind, Promise<string>>();
  return {
    route: async (kind) => {
      if (typeof token === "string") return { url, token };
      let pending = cached.get(kind);
      if (!pending) {
        pending = Promise.resolve(token(kind));
        cached.set(kind, pending);
        pending.catch(() => cached.delete(kind));
      }
      return { url, token: await pending };
    },
    // No answer: try the same endpoint again. A refused ticket: ask the token function again.
    reroute: (_target, code) => {
      if (code !== "ticket_invalid" || typeof token === "string") return code === TRANSPORT;
      cached.clear();
      return true;
    },
  };
}

export interface LocatorOptions {
  /** controld's base URL, e.g. `http://10.0.0.2:7080`. */
  readonly url: string;
  /** `mansio_<id>_<secret>`. */
  readonly apiKey: string;
  readonly tenant: string;
  readonly app: string;
  /** Ask controld to create the database on first use. */
  readonly create?: boolean;
  /**
   * The edge's base URL (e.g. `https://edge.example:7443`): when set, every request goes there and the
   * edge routes it by the ticket's vm; otherwise straight to the endpoint the locator answers.
   */
  readonly edge?: string;
  /** Scheme for endpoints answered as host:port (default http). */
  readonly scheme?: "http" | "https";
  /** One resolve's deadline, ms (default 3000). */
  readonly resolveTimeoutMs?: number;
  readonly fetch?: typeof fetch;
  readonly now?: () => number;
  readonly random?: () => number;
}

interface Route extends Target {
  readonly epoch: number;
  readonly refreshAt: number;
  readonly until: number;
}

/**
 * Routes through controld's locator (`POST /v1/resolve {"tenant","app"[,"create"][,"min_epoch"]}`):
 * one cached route for the client's database, refreshed in the background from 80 % ± 10 % of the
 * ticket TTL, single-flight, each resolve under its own deadline; `reroute` drops a stale route and
 * makes the next resolve ask for `min_epoch = epoch + 1` after `wrong_owner`/`fenced` (the locator then
 * reads the primary, whose answer ends the requirement even below it).
 */
export function locatorRouter(options: LocatorOptions): Router {
  const fetchFn = options.fetch ?? globalThis.fetch;
  const now = options.now ?? Date.now;
  const random = options.random ?? Math.random;
  const scheme = options.scheme ?? "http";
  const timeout = options.resolveTimeoutMs ?? 3_000;
  let route: Route | undefined;
  let minEpoch = 0;
  let flight: { promise: Promise<Route>; minEpoch: number } | undefined;

  const resolve = async (wanted: number): Promise<Route> => {
    const sent = now();
    let response: globalThis.Response;
    try {
      response = await fetchFn(`${options.url.replace(/\/+$/, "")}/v1/resolve`, {
        method: "POST",
        headers: { authorization: `Bearer ${options.apiKey}`, "content-type": "application/json" },
        body: JSON.stringify({
          tenant: options.tenant,
          app: options.app,
          ...(options.create ? { create: true } : {}),
          ...(wanted > 0 ? { min_epoch: wanted } : {}),
        }),
        signal: AbortSignal.timeout(timeout),
      });
    } catch (cause) {
      throw new RouteFailure(TRANSPORT, 0, String(cause));
    }
    let out: Record<string, unknown>;
    try {
      out = await response.json();
    } catch {
      throw new RouteFailure(TRANSPORT, response.status, "unreadable locator answer");
    }
    const code = typeof out.code === "string" ? out.code : TRANSPORT;
    if (code !== "ok") {
      const ms = typeof out.retry_after_ms === "number" && out.retry_after_ms > 0
        ? out.retry_after_ms
        : retryAfterMs(response.headers.get("retry-after"));
      throw new RouteFailure(code, response.status, String(out.message ?? code), ms);
    }
    const ttl = Number(out.ttl_ms);
    if (typeof out.endpoint !== "string" || typeof out.ticket !== "string" || !(ttl > 0)) {
      throw new RouteFailure(TRANSPORT, response.status, "locator answer ok without endpoint, ticket or ttl");
    }
    const base = options.edge ??
      (out.endpoint.includes("://") ? out.endpoint : `${scheme}://${out.endpoint}`);
    return {
      url: `${base.replace(/\/+$/, "")}/v1/ops`,
      token: out.ticket,
      epoch: Number(out.epoch ?? 0),
      refreshAt: refreshAt(sent, ttl, random()),
      until: usableUntil(sent, ttl),
    };
  };

  const start = (wanted: number) => {
    const promise = resolve(wanted).then((fresh) => {
      // The primary's answer is the truth, even below what was asked for.
      if (wanted > 0 && wanted >= minEpoch) minEpoch = 0;
      if (!route || fresh.epoch >= route.epoch) route = fresh;
      return fresh;
    }).finally(() => {
      if (flight?.promise === promise) flight = undefined;
    });
    flight = { promise, minEpoch: wanted };
    return promise;
  };

  return {
    route: async () => {
      const t = now();
      if (route && route.epoch >= minEpoch && t < route.until) {
        if (t >= route.refreshAt && !flight) start(0).catch(() => {}); // background refresh
        return route;
      }
      const current = flight && flight.minEpoch >= minEpoch ? flight.promise : start(minEpoch);
      return await current;
    },
    reroute: (target, code) => {
      if (route && route.token === target.token) route = undefined;
      minEpoch = Math.max(minEpoch, minEpochAfter(code, target.epoch ?? 0));
      return true;
    },
  };
}
