# 2026-11-04 — sqlbench: the run loop under these rules

Author: sqlbench agent. Code: `internal/bench/run.go`, `cmd/bench/sqlbench/main.go`.

- **Drain with one counter.** `outstanding` goes up when a job is queued or a retry timer is armed, and down
  when a worker finishes the job or the timer fires. The run is drained when it reaches 0. This replaces
  polling `inFlight`, `len(jobs)` and a WaitGroup of pending timers, which had gaps between them.
- **Panic forwarding without a result channel.** Workers and backoff timer callbacks `defer crash.guard()`.
  The first panic is kept with its stack as `*panicx.Panic`, the run is cancelled, and `Run` re-panics it in the
  caller once the workers stop. `realMain`'s `recover` reports it as a bug (exit 70) and logs the original
  stack. `actor.parallel` (used by `Directory.Grow`) does the same.
- **Cancellation finishes the files.** On Ctrl-C the dispatcher stops, no new retries are scheduled, pending
  backoffs are cancelled and recorded as never sent (map membership is ownership, so a timer that fired
  concurrently does nothing), in-flight attempts finish, and every CSV is flushed. `Run` returns `errs.Wrap("bench", ctx.Err(), ...)`, and realMain
  maps that to 130.
- **The user never sees a stack.** the reporter writes the detail record (chain, stack, log ID) to the log file
  only; stderr gets one line. Before the log file exists there is no ID to look up, so a bug prints its
  details instead. Tests: `TestFailureLogIDIsInTheLog`, `TestBugBeforeTheLogFileShowsDetails`.
