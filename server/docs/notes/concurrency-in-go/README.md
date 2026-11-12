# Concurrency in Go (Cox-Buday): error handling and logging, applied to mansio-server

Owner: coldload agent. Others: add notes in this folder (dated files) and a line below; do not rewrite this file.

Source: Katherine Cox-Buday, *Concurrency in Go* (O'Reilly, 2017), ch. 4 "Error Handling" and ch. 5 "Error
Propagation". Full research notes with source trust levels: `~/AI/writeups/cox_buday_concurrency_in_go_errors_logging.md`.
House style that turns it into Go templates: `~/AI/writeups/go-coding-style.md` §4–6.
Shared code in this repo: `internal/errs`, `internal/logx`, `internal/assert` (see `../../shared_state.md`).

## The approach in eight rules

1. **Error paths get the same care as the happy path.** Go has no exceptions on purpose; every error is a
   value someone must decide about.
2. **A goroutine does not decide what an error means.** It cannot see the whole program ("how many errors is
   too many?"). Printing the error and carrying on "hopes something is paying attention" — don't.
3. **Couple result and error in one value and send it to the parent** on the same channel as results (the
   Result pattern). The parent, which has the full picture, sets the policy: stop at the first error, stop
   after N, collect all.
4. **Whoever starts a goroutine makes sure it can stop**: pass `context.Context` (the book's `done` channel),
   cancel on early exit, buffer result channels to the sender count so no sender blocks after the reader
   leaves, and keep reading every result after the first error so nothing behind it is lost.
5. **Panics do not cross goroutines.** Recover inside the goroutine, put the panic in the result, re-panic in
   the parent so the top level's `recover` reports it as a bug. (Book: a panic is one of the "complete set of
   possible outcomes".)
6. **Every error a user sees carries four things**: what happened (the cause), when and where (UTC time,
   stack, context like key/path), a friendly one-line message that says whether retrying is safe, and an ID to
   find the full details in the log.
7. **Wrap at module boundaries only.** Every error leaving a package's public API is that package's
   well-formed error; an error that escapes without it is malformed, i.e. a bug. Capture the stack once, at
   the first well-formed layer. Keep the cause reachable (`Unwrap`) so `errors.Is` still matches sentinels.
8. **The top level classifies and logs once.** Outermost error well-formed (from a module it calls) → show its
   message + log ID, exit 1. Anything else (raw error, panic, failed assertion) → "this is a bug, report log
   ID x", exit 70. Never print a stack to the user; the log has it. Never log and also return.

**Logging** (book ch. 5 + `go-coding-style.md` §5): structured JSON lines (`log/slog`), UTC timestamps, a
short random log ID per reported error, the whole error chain and the origin stack in the log line. Only
integration code (cmd, adapters) logs; domain code returns errors and values describing what happened.

## How the shared packages implement it

| Rule | Code |
|---|---|
| 6, 7 | `errs.Wrap(module, err, "friendly msg; safe to retry", args...).With("key", v)`; `Error()` = `message: cause`; `Unwrap`; stack only at the origin; `errs.Chain`, `errs.Stack` for logs |
| 8 | `errs.Outermost(err)` checks only the outermost error; see `reporter` in `cmd/bench/coldload/main.go` |
| logging | `logx.New(w, level)` (JSON, UTC), `logx.NewID()` |
| contracts | `assert.That` for bugs only (see `../testing/`) |

## Where coldload applies it (file pointers)

- **Result pattern + parent policy + panic forwarding**: `internal/coldload/level.go` `RunLevel` — workers
  send `outcome{rec, panic}`; the parent reads every outcome, counts, then re-panics the first panic.
  `cmd/bench/coldload/seed.go` `upload` — workers send errors on a channel buffered to the job count; the parent
  cancels on the first error but drains all.
- **Panic in a background goroutine reaches the caller**: `internal/vactor/host.go` `activate` recovers into
  `entry.panic`; every `Get` waiting on that activation re-panics it (`entry.result`). Test:
  `TestActivationPanicReachesEveryCallerAndIsNotCached`.
- **A caller's cancel never fails other waiters**: activations run on their own goroutine with the host's
  context; `Close` cancels it (`TestWaiterCancelDoesNotFailOwner`, `TestCloseCancelsActivationInFlight`).
- **Module boundaries**: `objstore` (S3 adapter), `vactor`, `coldload` (domain validation), `cmd` (top
  level). Messages say what changed and whether retrying is safe, e.g.
  `cannot activate k: download failed; nothing cached, safe to retry: cannot download k: ...`.
- **Top level**: `cmd/bench/coldload/main.go` `realMain` → `reporter` (exit 0/1/2/70/130), `recover` → bug report,
  `signal.NotifyContext` for Ctrl-C (cleanup runs through normal defers, no `os.Exit` in a signal handler).
  Log: `results/coldload/<run>/coldload.log`; test `TestRealMainStoreDownIsAnExpectedErrorWithLogID` checks
  the printed log ID is in the log with its chain.
- **Records must not be lost silently**: the async CSV recorder counts drops, `Close` returns the first write
  error, and asserts `written + dropped == added`.

## Checklist for new code (either agent)

- [ ] Goroutines send `{result, err, panic}` to the parent; no `log`/`fmt.Println` of errors inside workers.
- [ ] Every goroutine has a way to stop (ctx) and never blocks on a send after the parent stops reading.
- [ ] Public functions return `errs.Wrap(<your module>, ...)` errors; sentinels still match with `errors.Is`.
- [ ] Messages say whether anything changed and whether retry is safe.
- [ ] Errors logged once, at the top, with a log ID; JSON, UTC.
- [ ] Panics recovered at goroutine boundaries and re-raised in the parent.

## Notes by others

(add `YYYY-MM-DD-<topic>.md` here and a line below)
- 2026-11-04 sqlbench: [run loop](2026-11-04-sqlbench-run-loop.md) — one outstanding counter for the drain, panic forwarding from timers, cancel finishes files, no stacks on stderr.
