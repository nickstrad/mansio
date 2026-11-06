# STATE — fencing: single-owner fencing per database key

> **ARCHIVED 2026-11-05.** Done; merged to main. Findings: `knowledge/fencing/`. Live coordination:
> `shared_state.md`.

Owner: the "fencing" agent (Opus). Branch `fencing`, worktree `/Users/nick/Software/mansio-server-wt/fencing`.
Shared log: `/Users/nick/Software/mansio/server/shared_state.md` (append only with `scripts/shared-note.sh`, absolute path).

## Goal

Make it impossible for two processes to accept writes for one database at the same time, even when routes are
stale (mansio `docs/architecture/scale-out-go-greenfield.md` §8 gap 3; option 4 §3.2–§3.4). The guarantee lives in
the process that hosts the database: durable local ownership state per key, epoch-checked admission, a fence
that drains and writes back before it acknowledges, and the storage backstop (contract 1 conditional publish).

## Plan

1. Pure domain core: local states, transition table `Step`, admission table `decide`, reconcile table. Spec tables.
2. `LocalStore` port + file adapter (temp + fsync + rename + dir fsync) + mem fake; contract suite on both.
3. `Table`: Admit/Permit (lock-free read, per-key counter), Claim, Fence, Publish, Reconcile, Forget, CheckActivation.
4. Ports + fakes in `ownertest`: `Placement` (Down/Delay/Stale/lost reply), contract-1 object store and per-process
   `WriteBack`, `LocalStore` mem fake.
5. Move protocol (`Mover`), resumable, with crash-point tests.
6. Model-based fuzz across N processes; `-race` regressions; benchmarks.
7. Knowledge: `knowledge/fencing/` (README + safety argument + Postgres placement note), index line.

## Status (newest first)

- 2026-11-05 10:55 — done: simplify pass committed; merging to main; state archived.
- 2026-11-05 10:50 — integration glue posted to the shared log (writeback host + front admitter).
- 2026-11-05 10:48 — fuzz (2.5M execs/90 s, clean after one harness finding), benchmarks (Admit 9 ns, 0 allocs),
  knowledge notes committed. Coverage 92.8% (`-coverpkg=./internal/owner/...`).
- 2026-11-05 10:35 — core, Table, FileStore, fakes, move protocol committed with spec tables, contract suite,
  crash-point and -race tests.
- 2026-11-05 10:22 — claim + API posted to shared log; starting the domain core.

## Owned files

`internal/owner/**`, `knowledge/fencing/**`, `STATE-fencing.md` (archive to `docs/state_files/STATE-fencing.md`).

## APIs others rely on

See the shared-log post "claim internal/owner; Admitter shape for front; Stamp addition to contract 1".

## API changes

- Contract 1 addition: `Stamp` (posted 10:22; writeback implemented it as `durable.CheckStamp`).
- `CheckActivation` takes a ctx; `Publish(ctx, key, epoch, fn)` returns the generation (posted 10:50).
