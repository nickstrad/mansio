# sqlbench: a retry registered after the cancel sweep was waited out

A check-then-act race in the bench run loop's cancel path, its fix, and the locking rule it gave.

Code: `internal/bench/run.go` (`maybeRetry`, `cancelBackoffs`).

**Symptom.** `TestCancelDoesNotWaitOutBackoffs` failed once in a full `go test -race ./...` under heavy load:
"took 6m16s: waited out hour-long backoffs". It passed alone and 40× under CPU load.

**Root cause (check-then-act).** `maybeRetry` read `stopping` at its top, then decided, took the budget, and
only then registered the backoff under `backoffMu`. On cancel the run does `stopping.Store(true)` then
`cancelBackoffs()` (sweep the map). A worker descheduled between its `stopping` check (false) and its
registration registered the backoff _after_ the sweep. Its `outstanding` count was never released, so the
drain waited for the timer. The delay is full-jittered by default (uniform in [0, 1 h]), which is why it took
6m16s and not an hour. The window is a few instructions, so only heavy load hit it. Retry hints
(`retry.HintOf`, `unavailable` in the default `RetryOn`) do not interact: actor errors carry no hint and the
test sets `RetryOn` explicitly.

**Fix.** Check `stopping` again under `backoffMu` before registering. The run sets `stopping` before the sweep
takes the lock, so either the sweep sees the backoff or the registration sees `stopping` (and records the
retry as never sent). Other interleavings were already safe: a timer that fires before the sweep removes
itself and loses or enqueues; one that fires after finds itself gone; a zero-delay retry is enqueued and
bounded by the drain.

**Test.** `TestRetryDecidedAsTheRunStopsIsNotWaitedOut` uses a test hook (`runner.beforeBackoff`) to stop the
run inside the window. It fails without the fix (outstanding 1, pending backoffs 1) and passes with it. The
original test passed `-count=200 -race` with 16 busy-loop CPU hogs running.

**Rule.** A flag that a sweep relies on must be read under the same lock as the registration the sweep scans;
a lock-free early check is only a fast path.
