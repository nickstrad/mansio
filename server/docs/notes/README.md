# Server notes

Design notes, measurements and traps for the Go server, one folder per area. Each folder's README lists its
topic docs. Repo-wide notes (the TypeScript compiler, the IR protocol from the SDK's side, decisions, gotchas)
are in [`docs/notes/`](../../../docs/notes/README.md).

- [front/](front/README.md): how a request reaches the actor that owns its database: agentd's network front
  (tickets, admission, wire codes, request ids, metrics), durable mode and admin controls, controld, the
  locator, the edge and the Go client.
- [ir/](ir/README.md): the IR protocol in Go: decoding,
  document schemas, the SQL compiler and the executor behind `POST /v1/ops`.
- [backpressure/](backpressure/README.md): how every queue is bounded and what is refused when it is full:
  admission, the edge, controld's pool budget, shutdown, the retry contract and the capacity table.
- [writeback/](writeback/README.md): warm → cold durability: immutable generations in S3, the manifest,
  snapshots, GC, S3 compatibility, the storage runbook and the version floor.
- [fencing/](fencing/README.md): single-owner fencing per database: epochs, the safety argument, Postgres
  placement and the lease options.
- [determinism/](determinism/README.md): the determinism seams and the single-node and cluster simulations.
- [coldload/](coldload/README.md): fetching a SQLite file from S3 into a ready actor, by size and concurrency,
  and the fetch bulkhead.
- [sqlbench/](sqlbench/README.md): one goroutine (actor) per SQLite database under load, compared with one
  global lock; commit cost with fsync.
- [testing/](testing/README.md): how the Go server is tested, the checklist, and traps of timing tests.
- [concurrency-in-go/](concurrency-in-go/README.md): the rules for errors, panics and logging across
  goroutines.

Related: the [coding style](../coding-style.md), the [capacity table](../capacity.md) and the
[control-plane plan](../control-plane-plan.md).
