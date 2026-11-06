# 2026-11-05 — what a handler executor needs from the `Executor` port

Author: front agent. Context: mansio's real workload is TypeScript handlers that read, decide and write inside
one transaction (`/Users/nick/Software/mansio/docs/architecture/scale-out-go-greenfield.md` §3). The handler
runtime (an embedded JS engine) is a separate, later spike. This note says how it slots in behind
`front.Executor` without changing the pipeline, framing, admission or wire codes. Nothing here is built or
measured.

## What stays

`front.Executor.Exec(ctx, Call) (Reply, error)` and its contract (`internal/front/ports.go`,
`fronttest.RunExecutorSuite`). The pipeline already gives the executor an authorized, admitted call with a
tenant, database, request id, kind, deadline, statement timeout and row cap. The error sentinels already cover
the handler outcomes: `ErrShed`, `ErrExpired`, `ErrNoReply` (ambiguous), `ErrStmtTimeout`, `ErrAborted`,
`ErrStatement` (the handler's own error → `bad_request`), anything else → `internal`.

## What changes

1. **The payload.** `Call.SQL`/`Args` become an operation: `{function, args}` (a compiled document request
   naming a handler). On the wire that is the reserved `op` field. `checkSQL` becomes "the function exists and
   the ticket's perms allow it" (a mutation needs `write`, a query `read`).
2. **Transaction scope.** A handler runs inside the actor's write transaction, not as one statement. The actor
   needs a second request type: "run this closure on your goroutine with your connection". Proposed additive
   actor API (not built): `Request.Run func(tx Conn) (Result, error)`, executed in `writeBatch` between
   `SAVEPOINT h_i` and `RELEASE h_i` (or `ROLLBACK TO h_i` on the handler's error, which fails only that
   handler).
3. **Synchronous db calls on the actor goroutine.** The handler's `db.get/query/insert/patch` must call the
   actor's connection directly and synchronously (no channel hop, no `await` over IPC), like Durable Objects'
   same-thread SQLite. So the JS engine runs **on the actor goroutine** for the handler's duration; the
   binding is a Go function the engine calls. The engine runtime is pooled per app and lane, not per database
   (an engine per database would dwarf the ~0.7 MB per-database budget).
4. **Savepoint per handler for group commit.** `BEGIN IMMEDIATE; for each handler: SAVEPOINT h_i; run;
   RELEASE | ROLLBACK TO h_i; COMMIT; reply to all` (output gate: no reply before the commit). One handler's
   failure rolls back only its savepoint; a statement timeout inside a handler interrupts SQLite, which rolls
   back the whole transaction, so batch-mates get `ErrAborted` exactly as today's `settle` does.
5. **Deadlines.** The statement timeout bounds each `db.*` call; the handler's total CPU needs its own bound
   (engine interrupt, e.g. goja `Interrupt`, QuickJS interrupt handler) mapped to `ErrStmtTimeout`. The
   executor must still return by `Deadline + grace` (port contract).
6. **Request ids.** Writes record `request_id` in the same transaction (option 4 §3.5) so `ambiguous`
   retries are answered "already applied". Not built; the front already requires `request_id` on writes.
7. **Read handlers** (queries) run outside the write transaction on the read pool, one handler per read
   connection, same synchronous binding.

## How to test it when it exists

Run `fronttest.RunExecutorSuite` against it with suite statements expressed as tiny handlers (get, scan, put,
slow, bad), plus handler-specific cases: a handler that throws rolls back only its writes; a batch of N
handlers commits once; a CPU-bound handler hits its interrupt.
