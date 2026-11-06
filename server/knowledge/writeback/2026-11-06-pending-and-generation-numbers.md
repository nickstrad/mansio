# 2026-11-06 — Publishes in doubt are kept for the grace window; uploads skip taken generation numbers

Author: DST-1 agent. Branch `fix/pending-gens`. Code: `internal/vactor/writeback.go` (`publishOnce`,
`settlePending`, `skipTaken`), `vactor.ErrInDoubt`. Found by the extended `FuzzDurableHostModel`.

## 1. A re-read showing our version proves nothing about a request in flight

Before: with `maxPending` (8) publishes in doubt, `settlePending` re-read the manifest and, if it still showed
`e.ver`, dropped them all ("none applied"). A request still in flight could land afterwards. The next publish
got 412, its re-read showed a manifest that is none of ours, and the key went stale and was superseded
(corpus `8da30814ac29f0c3`: 9 in-flight write-backs, a clock step, a tick). No restart needed.

Now a pending publish leaves `e.pending` only when it can no longer apply:
- a publish of ours conditional on `e.ver` succeeded, or a pending one was adopted (the version moved; every
  other one is conditional on the old version);
- or it was sent more than `Options.GC.Grace` ago. This is the same bound on delayed requests that the
  orphan sweeper already relies on to delete an upload (guide GC3). No new assumption.

The bound and liveness: with `maxPending` in doubt, `publish` re-reads first. If one of them applied, it is
adopted and all are cleared. If another writer moved the manifest, the result is `ErrStale`. Otherwise the
ones past the grace window are dropped, and if it is still full, nothing is sent: `ErrInDoubt`, safe to retry,
and the database stays warm. Write-backs of that key resume when one lands and is adopted, or at the latest
once the oldest is past the grace window (default 10 min). Getting there takes 8 ambiguous outcomes in a row
for one key, so it is rare.

Rejected:
- Fencing the doubtful publishes with a same-epoch stamp. The stamp is itself a conditional publish on `e.ver`.
  If it succeeds, it clears them just as the write-back's own publish would. If its outcome is ambiguous, it is
  one more pending publish. It adds nothing to the bound.
- Keeping only the newest 8. An older one landing is the same false stale.

## 2. Never reuse a tried generation number, across incarnations (guide I2)

`lastGen` starts at the manifest's generation on every activation: after a restart, a discard, or a
passivation. Numbers a previous activation of the key tried and left as orphans are at the same epoch, so
their object names collide. The upload loop gave up after 8 create-only PUTs, each one a full upload (input
`80281010101010101010a0208010`; also `043b1e2902c6c8f8`).

Now, on `ErrExists` for an object that is not ours, `skipTaken` HEADs `gen+1, gen+2, gen+4, …`
(`StatGeneration`) until one is missing. `lastGen` jumps there, so one more upload follows. Numbers may be
skipped, which is fine: generations need only rise (W3). Retention works from the names it saw published.

Why HEAD probes:
- LIST is off-limits on the agent (K2, and its credential has no LIST).
- Persisting `lastGen` would add a durable local write per upload, and it would still miss a lost VM's tries.
- Doing it in the reopen stamp would not cover discards and passivations.

Create-only PUTs (I1) keep the collision safe either way. This change is only about liveness and cost.

## Tests

- `TestPendingPublishesInDoubtAreKeptUntilGrace`: 8 in flight → `ErrInDoubt`; the oldest lands and is
  adopted; 8 failed → `ErrInDoubt` until the grace window passes, then the write-back goes through.
- `TestUploadSkipsGenerationsTakenByOrphans`: 40 orphans after a restart; one write-back publishes above them.
- `FuzzDurableHostModel`: the model keeps each pending publish's send time and the grace window, and every
  in-flight publish may land. The three inputs above are in the corpus.
