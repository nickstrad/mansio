# Commit cost with a real fsync: Linux in Docker Desktop vs macOS

What a durable SQLite commit costs when fsync really flushes, measured in Docker Desktop's Linux VM against
macOS.

Measured 2026-11-05 on an Apple M5 Max (18 cores), Docker Desktop's Linux VM (linux/arm64, 18 vCPU), Go
1.26.4, modernc.org/sqlite v1.61.1. **Indicative only:** the Linux disk is Docker Desktop's virtual disk on
the Mac's SSD, not a production host. Measure again on the target hardware before using these numbers for
capacity.

On macOS, SQLite's `fsync()` does not flush the drive cache (`F_FULLFSYNC` would; `PRAGMA fullfsync` is off),
so macOS FULL numbers understate the cost of a durable commit. On Linux, `fsync`/`fdatasync` is the durable
flush.

Command: `GOOS=linux GOARCH=arm64 go test -c -o actor.test ./internal/actor`, then in a `postgres:17`
container (Debian, used only as a Linux userland):
`TMPDIR=<dir> ./actor.test -test.run '^$' -test.bench
'BenchmarkWrites$' -test.benchtime 2s -test.cpu 18`. One
database, WAL, 288 concurrent writers (the benchmark's shape); "max_batch=1" = one transaction (one commit)
per write, "64" = group commit.

| ns per write         | macOS native | Linux, container overlay (VM ext4 disk) | Linux, bind mount (virtiofs → macOS) |
| -------------------- | -----------: | --------------------------------------: | -----------------------------------: |
| NORMAL, 1 txn each   |        9,845 |                                  10,062 |                               76,544 |
| NORMAL, group commit |        1,972 |                                   3,394 |                                5,455 |
| FULL, 1 txn each     |       59,831 |                              **93,491** |                              108,977 |
| FULL, group commit   |        2,490 |                               **4,330** |                                5,285 |

- **A durable commit costs ~93 µs on this Linux disk** (FULL, one txn each: ~10.7k commits/s per database),
  ~1.6× macOS's non-durable fsync. NORMAL (WAL: fsync only at checkpoint) is as cheap as on macOS.
- **Group commit hides it:** FULL with batching is 4.3 µs per write (~230k writes/s per database), 1.3×
  NORMAL. The actor's group commit is what makes FULL affordable; keep it.
- Bind mounts (virtiofs) are 1.2–7.6× slower and are not a realistic data directory; benchmarks in Docker
  Desktop should use the container's own disk or a named volume.
- Unknown: whether Docker Desktop's virtual disk honors flushes all the way to the SSD (the hypervisor may
  cache). Treat 93 µs as a lower bound for a real durable commit.

The [fencing safety argument](../fencing/safety-argument.md)'s fsync assumption (owner state files: temp,
fsync, rename, dir fsync) holds on Linux by construction; on macOS dev machines it does not, which is
acceptable for development only.
