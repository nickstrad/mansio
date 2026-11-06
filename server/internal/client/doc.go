// Package client is the Go client for mansio-server's data path (control-plane plan CP-6; Release It §P "Client"):
// it finds the VM that owns a database through the locator (controld's POST /v1/resolve, CP-5) and sends
// statements there (agentd's or the edge's POST /v1/query, contract 3).
//
// It is internal: the test and benchmark client (frontbench, the CP gates), and the reference for mansio's
// TypeScript SDK, which must behave the same way. A public Go SDK would wrap it once the wire carries document
// operations instead of SQL.
//
// What it guarantees per call (Do):
//
//   - One retry layer (RI R1). Shed, unavailable and aborted answers are retried by the retry.Policy: the wait
//     is max(Retry-After, full-jitter backoff) (RI R3), each retry spends the shared windowed retry.Budget
//     (RI R4), and no wait ends past the call's deadline.
//   - Rerouting. wrong_owner and fenced drop the cached route and re-resolve with min_epoch = epoch + 1 (the
//     locator then reads the primary); ticket_invalid and a VM that does not answer re-resolve plainly; the
//     locator's moving waits Retry-After. Bounded by MaxReroutes, backing off from the second. Rerouting
//     redirects load rather than repeating it, so it does not spend the budget.
//   - Deadlines. Every attempt sends X-Timeout-Ms a little below the time left (RI T4), so the agent answers
//     expired before the client stops listening.
//   - Writes apply at most once. A write gets a unique request_id (unless the caller gives one) and every
//     attempt sends the same id (CP-2), so a retry after a lost answer answers ok + duplicate.
//
// The location cache holds one route per database, refreshed in the background from RefreshAhead ± RefreshJitter
// of the ticket TTL, single-flight per database, each resolve under its own deadline (a caller that leaves
// does not cancel it for the others), bounded to MaxRoutes entries.
//
// Layout: decide.go is pure (answer → next step, Retry-After parsing, refresh times); routes.go is the cache;
// client.go is the HTTP adapter and the loop. Errors are wrapped once with module "client" around a *Error
// (errors.As) that names the final code and whether a write may have applied.
package client
