# 2026-11-05 — timing tests under load: count attempts, keep loose upper bounds (CP-8)

Author: CP-8 agent. Four tests flaked under `go test -race ./...` with other agents' builds running.

| Test | Cause | Fix |
|---|---|---|
| `workload` `TestEveryClassRunsOnRandomSchemas` | 30 ms statement timeout for every class; a fast statement that was descheduled timed out | Only expected-slow classes get 30 ms (they are meant to be cut); others must succeed within 5 s |
| `objstore` `TestSilentPeerFailsAtTheOpTimeout` | **A real bug**, ~1 in 10 even idle: `net/http`'s `ResponseHeaderTimeout` (= `OpTimeout`) matches `context.DeadlineExceeded`, so when it beat the attempt's own deadline (same duration) `Classify` said `Canceled` and the read was not retried | `attempt.end` marks any `DeadlineExceeded` while the caller still waits as `ErrTimeout` (Transient). Test now asserts 2 attempts (1 timeout + 1 retry) and only a loose "did not hang" bound |
| `faultnet` `TestFaultAppliesToTheNextNConnections`, `TestRefuseFailsFast` | Under load the proxy's reset can beat `connect()`, so `dial` itself failed and the helper called `t.Fatal` | A dial error counts as the connection failing (`dialRoundTrip`); "fast" = well under the 2 s read budget, not < 100 ms |
| `faultnet` `TestCloseEndsHeldConnections` | `time.Sleep(20ms)` "let the proxy accept" | Poll `p.Accepted()` |

Rules that came out of it:

- **Assert the mechanism, not the clock.** "One timeout, then a retry" is an attempt count; elapsed time is
  only a lower bound (it did wait) plus a generous upper bound (it did not hang).
- **Upper bounds get slack of seconds**, not milliseconds; a precise upper bound belongs in a benchmark.
- **Two timers with the same duration race.** If both can end an operation, both outcomes must classify the
  same way; a flaky timing test can be the only symptom.
- Reproduce with load: `for i in $(seq 16); do (timeout 150 sh -c 'while :; do :; done' &); done` while running
  `go test -race -count=4` on the packages.
