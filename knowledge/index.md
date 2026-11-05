# knowledge index

One line per topic folder or key note. Newest first within a section. Rules: `AGENTS.md`.

## Topics

- [testing/](testing/README.md): how we test (Aniche): domain vs integration, pre/postconditions + assertions + invariants (`internal/assert`), spec tables with on/off points, fakes with fault switches, contract suites, model-based fuzzing. Owner: coldload agent; others add dated notes.
- [concurrency-in-go/](concurrency-in-go/README.md): Cox-Buday error handling + logging (Result pattern, panic forwarding, wrap at module boundaries, log once with log ID); `internal/errs`, `internal/logx`. Owner: coldload agent; others add dated notes.
- [multi-agent-coordination/](multi-agent-coordination/README.md): how several agents share one repo: per-agent state files, append-only `shared_state.md`, `scripts/lock.sh` / `shared-note.sh` (copies inside). Owner: sqlbench agent.
- [sqlbench/](sqlbench/README.md): goroutine-per-database actors vs one global lock under generated load; throughput, latency, per-db cost, read pools, retries. Owner: sqlbench agent.
- [coldload/](coldload/README.md): SQLite files from SeaweedFS (S3) → ready virtual actors; fetch/open/warm timings by size and concurrency. Owner: coldload agent.
  - [2026-11-04 sizes](coldload/2026-11-04-sizes/README.md): fetch is ~all of ready time; ~250–290 MB/s cap; bound downloads by bytes in flight.
