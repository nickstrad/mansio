# 2026-11-05 — admission mutex profile: sharding by tenant is not needed yet (CP-8)

Author: CP-8 agent. Apple M5 Max, macOS 27.0, Go 1.26.4, GOMAXPROCS=18, load average ~3.

**Question:** does `front.Admission`'s single mutex show up in profiles, so admission should be sharded by
tenant? **Answer: no.** It is ~1–2 % of mutex delay and does not appear in the CPU profile; the hot lock is
SQLite's page-cache mutex.

## How

`frontbench run -mode pipeline` (in-process `front.Server` with its real `Admission`, ticket cache, actors on
64 seeded databases, 4 tenants, 10 % writes, 8 s), built with a temporary patch that turned on
`runtime.SetMutexProfileFraction(1)` and wrote CPU and mutex profiles (not committed; one-off):

```sh
frontbench seed -dir $D -dbs 64
FB_PROF=$OUT frontbench run -mode pipeline -dir $D -dbs 64 -tenants 4 -conc {64,512} -duration 8s -write 0.1
go tool pprof -top -cum frontbench $OUT/fb-mutex.out
```

| conc | ok/s | p50 | p99 | Admission in mutex delay | Admission in CPU |
|---:|---:|---:|---:|---:|---:|
| 64 | 190k | 189 µs | 1.9 ms | < 1.1 % (all of `front` 1.1 %) | not in profile |
| 512 | 194k | 1.75 ms | 13.2 ms | 1.8 % (`Acquire` 0.7 % + release 1.1 %) | not in profile |

Isolated, `BenchmarkAdmissionAcquireRelease` (18 goroutines on one key, all contending): 397 ns per
acquire+release → a ceiling of ~2.5 M admissions/s, ~13× the 194k req/s the whole pipeline reaches.

## What the profile does show (follow-up, not done)

At conc 512, **~51 % of all mutex delay is in SQLite's page cache** (`pcache1FetchWithMutex`,
`pcache1Unpin` under `sqlite3PagerGet` / `sqlite3VdbeHalt` / cursor close), across actors on *different*
databases. pcache1 takes the group mutex only when the page-cache group is shared (`pcache1FetchWithMutex`),
so modernc's build appears to share one group (and one lock) between all connections in the process (guess
from the stacks; not confirmed in the driver's build flags). Almost all the rest is under the same statement
calls (`database/sql` → modernc → SQLite); not broken down further.

Options to look at when per-VM throughput matters (none tried): modernc's `RegisterPageCache`
(`SQLITE_CONFIG_PCACHE2`, must run before the first open) with a per-connection cache, or compile-time
`SQLITE_ENABLE_MEMORY_MANAGEMENT`-related settings in the driver. Measure first: CPU profile share, not just
mutex delay.

## Decision

Do not shard admission. Revisit if a profile at production shape (http mode, many tenants) puts `Admission`
above ~10 % of mutex delay or in the CPU top 20.
