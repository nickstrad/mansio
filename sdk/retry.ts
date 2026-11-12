/**
 * The SDK's retry decisions, ported from mansio-server's `internal/retry` and `internal/client`
 * (`decide.go`): pure functions plus one piece of shared state, the windowed retry budget.
 *
 * - **Policy.** At most `maxAttempts` attempts per request (default 3); the wait before attempt `n + 1`
 *   is `max(Retry-After, full-jitter backoff)` with the backoff `base · 2^(n-1)` capped at `max`
 *   (50 ms, 5 s); no wait may end past the call's deadline.
 * - **Budget.** A token bucket: each first attempt adds `ratio` tokens (default 0.1), each retry takes
 *   one, the bucket holds at most `burst` (default 10). Credit expires, so a quiet hour banks at most
 *   `burst` retries for the storm that follows, and retries cannot multiply an outage. Shared by every
 *   call of one client.
 * - **What an answer means** (`decide`, `resolveStep`): the caller contract in `docs/retry-contract.md`,
 *   generated from mansio-server's `front.Contract` and shared with the Go client as test vectors
 *   (`ir/vectors/retry.json`, run by `tests/unit/sdk_retry_contract_test.ts`). `ok` done; `shed`,
 *   `unavailable`, `aborted` retry (with the same request body, so a mutation keeps its `requestId`);
 *   `wrong_owner`, `fenced`, `ticket_invalid` and no answer at all reroute (re-resolve, or fetch a fresh
 *   ticket) when the client can, else fail; everything else is final. `expired` is final too: its deadline
 *   is already spent, and resubmitting it would add load when the server is slowest.
 * - **Reroutes.** The first reroute of a call is free and waits a short jittered delay; each later one
 *   backs off and takes a budget token, so an agent going away is not answered with a burst of re-resolves.
 *   Controld's `moving` is never charged: it paces every caller of a database through a planned move.
 */

export interface RetryPolicy {
  /** Total attempts of one request, including the first; 1 = never retry. */
  readonly maxAttempts: number;
  /** First backoff, ms. */
  readonly baseMs: number;
  /** Cap on a jittered backoff, ms (a server hint may exceed it). */
  readonly maxMs: number;
}

export const DEFAULT_RETRY: RetryPolicy = { maxAttempts: 3, baseMs: 50, maxMs: 5_000 };

/** Longest Retry-After honored, ms (mansio-server `front.MaxRetryAfter`: one minute). */
export const MAX_RETRY_AFTER_MS = 60_000;

/** Most reroutes (re-resolves, fresh tickets) of one call. */
export const DEFAULT_MAX_REROUTES = 5;

/**
 * The wait before attempt `attempt + 1` (attempt 1 just failed): full jitter over the capped exponential
 * backoff, never below the server's hint. `u` is a uniform draw in [0, 1).
 */
export function backoff(policy: RetryPolicy, attempt: number, hintMs: number, u: number): number {
  const ceiling = Math.min(policy.maxMs, policy.baseMs * 2 ** Math.max(0, attempt - 1));
  return Math.max(hintMs, Math.floor(u * ceiling));
}

/** The server's Retry-After header (whole seconds) as ms; anything unparsable is no hint (0). */
export function retryAfterMs(header: string | null): number {
  if (header === null || !/^\d+$/.test(header.trim())) return 0;
  return Math.min(Number(header.trim()) * 1000, MAX_RETRY_AFTER_MS);
}

/** The windowed retry budget. Safe to share between clients. */
export class RetryBudget {
  private spent = 0;
  constructor(readonly ratio = 0.1, readonly burst = 10) {}
  /** Records a first attempt: refills the bucket by `ratio`. */
  first(): void {
    this.spent = Math.max(this.spent - this.ratio, 0);
  }
  /** Takes one retry token, if there is one. */
  tryRetry(): boolean {
    if (this.spent + 1 > this.burst + 1e-9) return false;
    this.spent++;
    return true;
  }
}

/** What the client does after one answer. */
export type Step = "done" | "fail" | "retry" | "reroute";

/** The code of an attempt that got no answer (connect failure, reset, unreadable body). */
export const TRANSPORT = "transport";

/** The next step after an answer with wire tag `code` (`"ok"` for a 200). */
export function decide(code: string): Step {
  switch (code) {
    case "ok":
      return "done";
    case "shed":
    case "unavailable":
    case "aborted":
      return "retry";
    case "wrong_owner":
    case "fenced":
    case "ticket_invalid":
    case TRANSPORT:
      return "reroute";
  }
  return "fail";
}

/** The next step after a failed resolve with controld's code `code` (never `"done"`). */
export function resolveStep(code: string): Step {
  switch (code) {
    case "moving":
      return "reroute";
    case "unavailable":
    case "shed":
    case "no_capacity":
    case "timeout":
    case TRANSPORT:
      return "retry";
  }
  return "fail";
}

/** Whether a mutation that ended with `code` may have committed (the caller is told; the requestId is safe to reuse). */
export function mayHaveApplied(code: string): boolean {
  return code === TRANSPORT || code === "expired";
}

/** The `min_epoch` of the re-resolve after a reroute from a route at `epoch` (0 = a plain re-resolve). */
export function minEpochAfter(code: string, epoch: number): number {
  return code === "wrong_owner" || code === "fenced" ? epoch + 1 : 0;
}

/** X-Timeout-Ms for `remaining` ms left: a little less, so the server answers before the client gives up. */
export function timeoutHeader(remainingMs: number): number {
  return Math.max(1, Math.floor(remainingMs - Math.min(remainingMs / 10, 100)));
}

/**
 * When a route fetched at `fetched` with ticket TTL `ttl` should be refreshed in the background:
 * `ahead · ttl`, moved by up to `± jitter · ttl` (`u` in [0, 1)).
 */
export function refreshAt(fetched: number, ttl: number, u: number, ahead = 0.8, jitter = 0.1): number {
  return fetched + (ahead + jitter * (2 * u - 1)) * ttl;
}

/** When a route stops being used: its TTL from when the resolve was sent, minus min(ttl/10, 1 s). */
export function usableUntil(sent: number, ttl: number): number {
  return sent + ttl - Math.min(ttl / 10, 1_000);
}
