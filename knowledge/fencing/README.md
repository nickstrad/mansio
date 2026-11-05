# fencing

Owner: fencing agent (state: `../../docs/state_files/STATE-fencing.md`, archived). Code: `internal/owner`,
fakes and the contract suite in `internal/owner/ownertest`.

Single-owner fencing per database key: at most one process admits writes for a database, even when routes
are stale (ts-db `scale-out-go-greenfield.md` §8 gap 3; option 4 §3.2–§3.4).

## Findings (newest first)

- 2026-11-05 [safety argument](2026-11-05-safety-argument.md): what guarantees one owner (epochs + local
  durable state + fence-before-flip + storage stamp), under which assumptions (linearizable authority reads,
  atomic conditional publish, fsync, operator-attested VM loss; **no clocks**), and what breaks it. Mutation
  check: without `Stamp`, the model fuzzer finds a forked history in 14 s.
- 2026-11-05 [Postgres placement adapter](2026-11-05-postgres-placement.md): row, CAS as one conditional
  UPDATE, primary-only reads, invariants as a trigger, lost replies handled by resumable moves.
- 2026-11-05 **Admit is ~9 ns, 0 allocs** (Apple M5 Max, `go test -bench=Admit ./internal/owner`): 9.0 ns
  admit+done on one key, 8.7 ns rejected, 3.2 ns/op with 18 goroutines over 4096 keys, 48 ns/op with 18
  goroutines on one key (one shared counter cache line). `sync.Map` + two atomics per key. A sharded
  `RWMutex` map was slower (11.5–13 ns many keys: reader counts contend).
- 2026-11-05 **`assert.That` arguments allocate on hot paths.** `assert.That(n >= 0, "%q", s.key)` boxes `s.key`
  into `any` on every call, even when the condition holds: 16 B, 1 alloc per Admit (9 → 17 ns). Guard it:
  `if n < 0 { assert.That(false, ...) }`. Applies to every hot-path assertion in the repo.
- 2026-11-05 **The contract suite found a file-name bug at the on point.** A 120-byte key (the max) has a
  240-char hex name; the temp file `<hex>.tmp-<12>` passed 255 bytes. Temp files are now `.tmp-<random>`.
- 2026-11-05 **Fetch only when a claim activates.** Re-fetching on an idempotent Claim replaces the warm copy
  and drops acknowledged, unpublished writes (model fuzz, first run; regression input committed).

## How it fits (integration calls)

| Caller | Call |
|---|---|
| host start | `owner.Open(ctx, Config{Self, Store: FileStore, Placement, WriteBack})`, then `ReconcileAll(ctx)` (keys stay closed if it fails; retry) |
| control plane: place / move target | `Claim(ctx, key)`; the host fetches and activates only if the state changed, after `CheckActivation(ctx, key, epoch, manifest)` |
| front, per request | `p, err := Admit(key, ticketEpoch)` → map `ErrWrongOwner`→`wrong_owner`, `ErrMoving`/`ErrFenced`→`fenced`, `ErrUnavailable`→retry-later; executor calls `p.Done()` after commit/failure |
| host periodic / passivate write-back | `Publish(ctx, key, epoch, fn)` (serialized, drained by Fence; `ErrSuperseded` ⇒ discard the warm copy) |
| control plane: move source | `Fence(ctx, key) → Ack` (idempotent; retry on error) |
| control plane | `Mover{Placement, Agent}.Move(ctx, key, dst)`, resumable |

## Tests and commands

```sh
go test -race ./internal/owner/...           # spec tables, contract suite (file + mem), crash points, races
go test -tags noassert ./internal/owner/...
scripts/lock.sh results/.bench-lock go test -run='^$' -fuzz=FuzzOwnershipModel -fuzztime=90s ./internal/owner
scripts/lock.sh results/.bench-lock go test -run='^$' -bench=Admit ./internal/owner
```

Open: request-id de-duplication (option 4 §3.5) lives in the database, not here; leases (time-bounded
zombie window) not built; Postgres adapter not built.
