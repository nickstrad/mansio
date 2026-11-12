# The Go client

The locator-routed Go client: its retry and reroute loop, request ids for writes, and the gate results through
moves and an S3 outage.

Code: `internal/client` (`decide.go` pure, `routes.go` cache, `client.go` HTTP + loop), `internal/outcome`
(`Unavailable`), `internal/retry` (`HintOf`, default `RetryOn` += unavailable),
`cmd/bench/frontbench/locator.go` (`frontbench
locator`), `cmd/controld/client_gate_test.go`
(`TestClientGate`).

## Where it lives

`internal/client`, not a public package: it is the test and benchmark client and the reference behavior for
mansio's TypeScript SDK. A public Go SDK would wrap it once the wire carries document operations instead of
SQL.

## The loop (one call, `Client.Do`)

```
route ← cache (single-flight resolve per db; refreshed in the background at 80% ± 10% of the ticket TTL;
        unusable after TTL − min(TTL/10, 1 s); ≤ 10 000 dbs)
POST {endpoint}/v1/query  Authorization: Bearer <ticket>  X-Timeout-Ms: time left − min(10%, 100 ms)
ok                                → done
wrong_owner / fenced              → drop route, next load min_epoch = epoch+1 (fenced: wait Retry-After)
ticket_invalid / no answer        → drop route, re-resolve
shed / unavailable / aborted      → retry.Policy.Decide: wait max(Retry-After, full-jitter backoff), spend Budget
locator moving                    → wait Retry-After, re-resolve (no budget)
locator unavailable / timeout / no answer → like shed (budget)
bad_request, stmt_timeout, internal, expired, locator 4xx → fail (policy RetryOn can opt in)
```

- **Rerouting is bounded, not budgeted.** At most `MaxReroutes` (5) per call; the first is immediate (or waits
  Retry-After), later ones back off with the policy's delay. Rerouting redirects load rather than repeating
  it, so it does not spend the retry budget (the budget exists to stop retries amplifying load on the same
  server).
- **No wait ends past the call's deadline** (ctx's, or `Timeout` 5 s): the call returns the last answer.
- **Writes:** a unique `request_id` (`c<12 hex>-<seq base36>`) unless the caller gives one; every attempt
  sends the same body bytes. `*client.Error.MayHaveApplied` marks a write with no answer, `internal` or
  ambiguous `expired`.
- **TLS:** `Scheme: "https"` for endpoints returned as `host:port`, `CAFile` for a private CA
  (`tlsx.ClientConfig`); endpoints that carry a scheme (an edge URL) are used as they are.

## Decision: the primary's answer ends a min_epoch requirement

The first gate run showed 25k cache misses in 8 s (controld: 8 794 primary loads, 7 cache hits). Cause: the
locator routes to a new owner as soon as the placement flips, **before the new owner's claim lands**; that
owner answers `wrong_owner` at the _current_ epoch for a few ms. The client then asked for
`min_epoch = epoch + 1`, which the primary can never satisfy, and the requirement stuck. Now a load made with
`min_epoch` (a primary read) ends the requirement whatever epoch it returns; the reroute bound and backoff
cover the claim's last milliseconds (with 3 re-resolves one write in 170k still failed with `wrong_owner`;
with 5, none in three runs).

The locator now also answers `moving` while the registry intent is still `moving`/`creating`, so `wrong_owner`
keeps meaning "this epoch is over"; the registry's flip to active notifies the locator (see
[locator, controld and edge fixes](locator-controld-edge-fixes.md)). The rule above stays for the cases that
still give `wrong_owner` at the current epoch.

## Gate results (2026-11-05, M5 Max; Postgres 17 + SeaweedFS 4.47 in Docker; two agentd + controld; 3 runs)

| Gate                                                                                                              | Test                                                                                          | Result                                                                                                                                                                  |
| ----------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| frontbench through the locator, resolve hit/miss latency                                                          | `TestClientGate` (frontbench `locator`, conc 16, 4 dbs, 30% writes, 8 s, ticket TTL 5 s)      | 188–195k requests, 23.5–24.4k ok/s, p50 223–246 µs, p99 ≈ 950 µs; resolve hit p50 0.2 µs, miss p50 0.65–1.1 ms (p99 1.7–2.3 ms); 102–116 misses (TTL refreshes + moves) |
| error rate during moves                                                                                           | same, a move every 1 s (6 moves)                                                              | 139–144k requests overlapped a move (or the 2 s after it): **0 failed**; 123–135 re-resolves                                                                            |
| S3 breaker tripped → first retry waits ≥ Retry-After                                                              | `TestClientGate` part 2 (agent admin `breaker/s3:trip` on a passivated db, reset after 1.5 s) | first answer `unavailable`, Retry-After 5 s; the client waited 5 s; the call succeeded on attempt 2 after 5.008 s                                                       |
| `retry-storm` retries/first ≤ ratio per 1 s window                                                                | sqlbench, `configs/retry-storm.json` trimmed to its three `good` phases (manual)              | 999 999 firsts, 85 615 retries (0.086 overall); 36 windows, per-window 0.052–0.093, all ≤ 0.1                                                                           |
| unit: decisions, Retry-After, refresh jitter, single-flight, reroute min_epoch, same request id, budget, deadline | `internal/client` tests (`-race`, noassert)                                                   | pass                                                                                                                                                                    |

Per-window query (DuckDB, `queries/views.sql`):

```sql
with w as (select floor(sched_ms/1000)::int sec, count(*) filter (where attempt=1) firsts,
                  count(*) filter (where attempt>1) retries
           from requests_csv('<out>/requests.csv') where client='good' group by 1)
select max(retries::double/firsts), count(*) filter (where retries > 10 + 0.1*firsts) from w where firsts > 0;
```

Manual run:
`MANSIO_ADMIN_TOKEN=… frontbench locator -locator http://controld:7080 -tenant fb -dbs 16 -setup
-move-every 2s -out results/locator.json`
(`MANSIO_API_KEY` or `-setup` creates one).

## Also

- `internal/bench` passes `retry.HintOf(res.Err)` as `ServerHint`; actor errors carry no hint today (0).
- `front.WireArgs` and `tlsx.ClientConfig` are new shared helpers.
- Not implemented: per-call metrics in the client beyond hooks.
