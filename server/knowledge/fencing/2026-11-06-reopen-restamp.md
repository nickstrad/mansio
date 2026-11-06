# 2026-11-06 — A restart re-stamps the manifest before it admits (zombie publishes)

Author: DST-1 agent. Branch `fix/restart-zombie`. Code: `owner.Table.Reconcile` (`internal/owner/table.go`),
`durable.Manifest.Seal` and `durable.Stamp` (`internal/durable/durable.go`).

## The bug (found by the DST-1 sim, seeds 180 and 232 with zombies on)

A process crashes while a manifest publish is in flight: the PUT `If-Match: v` has left, its reply never came.
The restarted process reopens at the **same epoch**, reads the manifest (still `v`) and serves. Then the old
PUT lands: its condition still holds, so it applies. The new process's next publish gets 412, its re-read
shows a manifest that is none of its own (guide R4), it goes stale and the key is fenced as superseded. Its
newer acknowledged writes are never published.

## The fix

On `EvReopen`, `Reconcile` now stamps the manifest at our own epoch before it opens the key (the same
`t.stamp` loop a claim uses: Head, conditional `WriteBack.Stamp`, fenced by `CheckStamp`). A stamp is now
**always a new version**, also at the stored epoch: `Manifest.Seal` rises on every stamp, so the bytes and the
ETag change (guide W3; `(Epoch, Generation, Seal)` only rises). Every publish conditional on an earlier
version, the crashed incarnation's included, fails with 412 when it lands. If the stamp fails, the key stays
closed (safe to retry), as for a claim.

## Why not reopen at a new epoch

- The epoch is the authority's (placement). Bumping it on every restart makes every agent a placement
  writer, invalidates every ticket and locator route on each restart, and adds a CAS per key to startup.
- It is not needed: the zombie is our own epoch's request. A version change refuses it as well as an epoch
  change would, with no new authority write.

## Multi-node (moves, DST-2)

- Reopen happens only when placement names us at our local epoch and the manifest epoch is not higher; the
  stamp cannot raise anything past the authority.
- A superseded node cannot use the stamp to keep publishing: if a new owner has stamped a higher epoch,
  `CheckStamp` refuses ours, the key stays closed, and the next round supersedes it. Epochs, tickets and
  placement are unchanged (S1, S2 of the safety argument hold as before).
- A zombie's generation **upload** is harmless: an object nobody names, swept as an orphan after the grace.

Cost: one Head and one conditional PUT per owned key at startup.

## Tests

- `TestReopenStampFencesTheCrashedIncarnationsInFlightPublish` (owner): a publish conditional on the
  pre-restart version is refused after Reconcile; the next write publishes; the epoch stays 1.
- `TestStamp` (durable): a same-epoch stamp is a new version with `Seal+1`.
- `FuzzDurableHostModel`: ops 230..241 leave a write-back's publish in flight (`LandLate`); a crash
  re-stamps like agentd; seed "in flight, crash, restamp" fails without the fix.
- The sim now lands dead processes' publishes by default (`sim.Config.NoZombies` turns it off). Its checker judges a
  restart's restore at each key's reopen stamp, not at the restart: a zombie landing in between (the
  reconcile stalled, the key closed) is what the restart starts from (seed 984). 1000 seeds pass.

## Open finding (not fixed here): the host forgets in-flight publishes when it settles them

Same incarnation, no restart. `vactor.settlePending` runs once `maxPending` (8) publishes are in doubt; if the
re-read shows the version unchanged it drops them ("none applied"). A request still in flight can land after
that: the next publish gets 412, the re-read shows a manifest that is none of ours, the key goes stale and is
superseded. This contradicts `publish`'s own rule ("only a successful conditional publish proves none did,
nor can"). The extended fuzz model found it in 37 s (corpus `8da30814ac29f0c3`:
9 in-flight write-backs, one clock step, a tick). The model now assumes a settled publish never lands
(`SetLandIf`). A fix would bound pending publishes by age (the guide's grace window), not by count.

Also seen once in 150 s of fuzzing, and also present on main with main's model: after a crash, a write-back
under `LoseUploadReply` fails with "generation object already exists", probably because an orphan of the dead process holds
that generation number (`FuzzDurableHostModel` input kept outside the corpus). Not investigated further.
