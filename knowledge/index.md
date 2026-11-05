# knowledge index

One line per topic folder or key note. Newest first within a section. Rules: `AGENTS.md`.

## Topics

- [coding style](../docs/coding-style.md): one-page rules for all code, summarizing the guides in [docs/books/](../docs/books/README.md) (Release It!, Art of PostgreSQL, Object Storage, plus testing and concurrency here). Read before planning. Owner: coordinator.
- [control-plane plan](../docs/control-plane-plan.md): 2026-11-05 standing after writeback/fencing/front landed, the three flows (route, new db, place+warm cold db), and the build order to a client that reaches the right process. Owner: coordinator.
- [front/](front/README.md): the agent's network front: Ed25519 tickets, per-tenant/per-db admission, `Admitter`/`Executor` ports, wire codes; pipeline −7% vs in-process actor, loopback HTTP ~7× slower; ticket verify 26 µs (cached 53 ns). Owner: front agent.
  - [2026-11-05 agentd durable mode](front/2026-11-05-agentd-durable-mode.md) (CP-1): vactor + owner + S3 wired; lifecycle endpoints; kill -9 / S3 outage / SIGTERM gates pass.
  - [2026-11-05 request ids](front/2026-11-05-request-ids.md) (CP-2): exactly-once writes per database via `_tsdb_requests`; duplicate → `ok` + `duplicate: true`; +4 µs/write at batch 64.
- [writeback/](writeback/README.md): warm → cold durability: immutable generations + conditional manifest (`internal/durable`), write-back before passivation, RPO by interval/commits, idle + LRU budget eviction; snapshot by backup API on a reader (no write stall, CP-8); TestS3Compat passes on SeaweedFS (single and 2-filer cluster) and MinIO. Owner: writeback agent.
- [fencing/](fencing/README.md): single-owner fencing per database (`internal/owner`): epochs, durable local ACTIVE/FENCED, drain-then-write-back fence, storage stamp backstop, resumable moves; safety argument, Postgres placement note, Admit 9 ns. Owner: fencing agent.
- [testing/](testing/README.md): how we test (Aniche): domain vs integration, pre/postconditions + assertions + invariants (`internal/assert`), spec tables with on/off points, fakes with fault switches, contract suites, model-based fuzzing. Owner: coldload agent; others add dated notes.
- [concurrency-in-go/](concurrency-in-go/README.md): Cox-Buday error handling + logging (Result pattern, panic forwarding, wrap at module boundaries, log once with log ID); `internal/errs`, `internal/logx`. Owner: coldload agent; others add dated notes.
- [multi-agent-coordination/](multi-agent-coordination/README.md): how several agents share one repo: per-agent state files, append-only `shared_state.md`, `scripts/lock.sh` / `shared-note.sh` (copies inside). Owner: sqlbench agent.
- [sqlbench/](sqlbench/README.md): goroutine-per-database actors vs one global lock under generated load; throughput, latency, per-db cost, read pools, retries. Owner: sqlbench agent.
- [coldload/](coldload/README.md): SQLite files from SeaweedFS (S3) → ready virtual actors; fetch/open/warm timings by size and concurrency. Owner: coldload agent.
  - [2026-11-04 sizes](coldload/2026-11-04-sizes/README.md): fetch is ~all of ready time; ~250–290 MB/s cap; bound downloads by bytes in flight.
