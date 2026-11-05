# 2026-11-05 — Why at most one process writes a database: the safety argument

Author: fencing agent. Code: `internal/owner` (`state.go` tables, `table.go`, `move.go`). Tests that check each
claim are named in brackets.

## What is guaranteed

For each database key:

- **S1, one admitter per epoch.** At most one process admits requests at a given epoch. If every process the
  authority gave up on was really dead (operator-attested VM loss, option 4 M5), then at most one *live*
  process admits the key at all, and it is the authority's owner at the authority's epoch.
  [`FuzzOwnershipModel` invariant I1, `TestAdmitRacingFenceNoPermitDuringFinalWriteBack`]
- **S2, a linear published history.** Each manifest publish extends the previous one: no fork, no published
  write dropped, no write published twice. This holds **even when S1's assumption is broken** (a restore
  around a live owner, a stale authority read). [I2, the `rogue-restore` op, `TestStorageBackstopLowerEpochOwnerFencesItself`]
- **S3, moves keep acknowledged writes.** Every write a source admitted before its fence acknowledgement is in
  the generation the target starts from. With moves and restarts, an acknowledged write is lost only if an
  operator gave up a dead VM's unpublished writes (VM-loss restore). [I3, `TestMoveCrashPoints`]

## Why (the argument)

1. **Epochs are unique per owner.** The authority (Placement) changes a row only by CAS. It changes the owner
   only together with an epoch increase, and its epochs never decrease. So epoch E names exactly one owner.
2. **Admission is local and exact.** `Admit(key, e)` succeeds only while the local state is ACTIVE at exactly e
   (`decide`). A process becomes ACTIVE at E only through `Claim`, after a linearizable authority read said
   "owner = me, ACTIVE, epoch E", or through `Reconcile` after a restart, when the authority confirmed the same
   epoch E it had recorded on disk. Together with 1, that gives S1 for each epoch.
3. **A fence ends an epoch on its process, durably.** `Fence` installs FENCING (from then on every Admit
   fails), waits until the per-key in-flight counter reaches 0, runs the final write-back, records FENCED on
   disk (temp file, fsync, rename, directory fsync), and only then returns the Ack. A fenced epoch is never
   reopened (`Step`: fenced + claim at the same epoch is `ErrFenced`). The counter and the state are both
   sequentially consistent atomics. Admit increments the counter before it reads the state. Fence writes the
   state before it reads the counter. So any Admit that saw ACTIVE is counted before Fence reads the counter.
4. **The authority flips only on an Ack.** The move is CAS ACTIVE→MOVING, Fence, then CAS MOVING(src, E) →
   ACTIVE(dst, E+1, Ack.Gen). So while src may still admit at E, nobody holds E+1. Once dst holds E+1, src is
   durably fenced, and every write src admitted is in Ack.Gen, which dst starts from (S3).
5. **A restart fails closed.** Every ACTIVE record loads as CLOSED and admits nothing until `Reconcile` has
   read the authority and the manifest. A process that crashed mid-move comes back FENCING (`reconcileEvent`).
6. **Storage is the backstop (contract 1 + Stamp).** Every publish carries the publisher's epoch and is
   conditional. A new owner *stamps* the manifest with its epoch before it admits anything. After that, every
   publish at a lower epoch fails with `ErrStale`. The zombie then fences itself durably (`ErrSuperseded`). If
   the zombie published before the stamp, the new owner starts from the zombie's generation (`Claim` takes the
   manifest's generation when it is ≥ the authority's). Either way, the history stays linear (S2).

## Assumptions (what is not verified here)

| Assumption | Needed for | If it breaks |
|---|---|---|
| Authority CAS is atomic and keeps epochs monotone, one owner per epoch | S1, S3 | S1 can fail; S2 still holds (storage) |
| `Placement.Get` is linearizable for Claim and Reconcile (read the primary) | S1 | A stale read could reopen a zombie. Reconcile's manifest check catches it once the new owner has stamped; before that stamp, S1 can fail for a short window. S2 still holds |
| Contract-1 conditional publish is atomic (S3 `If-Match`, SeaweedFS honors it) | S2 | Forks are possible; nothing above storage can prevent them |
| `fsync` plus `rename` plus directory `fsync` is durable (on macOS, `fsync` does not flush the drive cache; `F_FULLFSYNC` would) | 5, 3 | After power loss a FENCED record could revert to ACTIVE. The restart still comes back CLOSED and reconciles with the authority, which says MOVING or a higher epoch, so it stays closed. Safe, unless the authority is also behind |
| VM-loss restore is done only for dead VMs (operator attests) | S1 (live), S3 | A partitioned "dead" owner keeps admitting at E. Its next publish fails after the new owner's stamp, and it fences itself. Writes it acknowledged and never published are lost (allowed by the model only for rogue restores) |
| The executor calls `Permit.Done` only after the request's write committed to the local copy, and the final write-back snapshots the local copy after that | S3 | Drained but uncommitted writes could miss the final generation |
| Publishes for a key go through `Table.Publish` (serialized, drained by Fence) | 6 (ErrStale ⇒ superseded) | A same-process race on the manifest could be mistaken for "superseded" and fence the real owner (safe, but unavailable) |

**No clocks.** Nothing depends on time: there are no leases and no TTLs in ownership. Ticket expiry (front) is
for authentication only. The price is that a partitioned, live owner keeps serving its epoch until it is
fenced or its publish fails. The alternative is a lease: the owner stops if it cannot renew within T, and a
restore waits T plus the worst clock drift. That would bound the window but would make safety depend on clocks.
Not built; it is listed as an option.

## What would break it (found while building)

- Without the Stamp, a zombie wins one more publish at E after the flip. The new owner's first publish then
  fails as "changed since read". Mutation test, 2026-11-05: removing the `Stamp` call in `Claim` made
  `FuzzOwnershipModel` report `published history of k0 forked: [w1] → []` within 14 s.
- Re-fetching the manifest on an idempotent Claim (the key is already ACTIVE here) replaces the warm local
  copy and drops acknowledged writes that are not yet published. The fuzz model found this in the test
  harness on its first run (`testdata/fuzz/FuzzOwnershipModel/claim-refetch-drops-unpublished-writes`). Rule
  for the host: fetch only when a claim changes the key's state on this process.
