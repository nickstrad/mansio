# Publishes in doubt and generation numbers

How the write-back host keeps publishes whose outcome it never saw until they can no longer apply, and how an
upload skips generation numbers that orphans already took.

Code: `internal/vactor/writeback.go` (`publishOnce`, `settlePending`, `skipTaken`), `vactor.ErrInDoubt`. Found
by the extended `FuzzDurableHostModel`.

## 1. A re-read showing our version proves nothing about a request in flight

Before: with `maxPending` (8) publishes in doubt, `settlePending` re-read the manifest and, if it still showed
`e.ver`, dropped them all ("none applied"). A request still in flight could land afterwards. The next publish
got 412, its re-read showed a manifest that is none of ours, and the key went stale and was superseded (corpus
`8da30814ac29f0c3`: 9 in-flight write-backs, a clock step, a tick). No restart needed.

Now a pending publish leaves `e.pending` only when it can no longer apply:

- a publish of ours conditional on `e.ver` succeeded, or a pending one was adopted (the version moved; every
  other one is conditional on the old version);
- or it was sent more than `Options.GC.Grace` ago. This is the same bound on delayed requests that the orphan
  sweeper already relies on to delete an upload (S3 never applies a request later than the grace). No new
  assumption.

The bound and liveness: with `maxPending` in doubt, `publish` re-reads first. If one of them applied, it is
adopted and all are cleared. If another writer moved the manifest, the result is `ErrStale`. Otherwise the
ones past the grace window are dropped, and if it is still full, nothing is sent: `ErrInDoubt`, safe to retry,
and the database stays warm. Write-backs of that key resume when one lands and is adopted, or at the latest
once the oldest is past the grace window (default 10 min). Getting there takes 8 ambiguous outcomes in a row
for one key, so it is rare.

The gap after an outage: during an S3 brownout, every tick's write-back of a key can end in doubt. After about
8 such ticks the key hits `ErrInDoubt`, and once S3 recovers it can stay unable to publish for up to the grace
window (10 min), unless one of the in-flight publishes lands and is adopted. Its writes stay warm and
acknowledged but not durable, so that time counts against the RPO. Counter:
`mansio_vactor_events_total{event="publish_in_doubt"}` (`Stats.PublishesInDoubt`). agentd answers `ErrInDoubt`
503 unavailable, with `Retry-After` set to when the oldest pending publish passes the grace window (capped at
60 s), on the request path and on fence/release/move/backup.

How the grace bound holds: the client gives up on a publish after `OpTimeout` (5 s), and on a write-back after
`WriteBackTimeout` (agentd: 60 s), far inside 10 min. That S3 never applies a request later than that is the
sweeper's assumption about the server side. The `now` recorded for a pending publish is taken just before it
is sent, so it is dropped early rather than late.

Rejected:

- Fencing the doubtful publishes with a same-epoch stamp. The stamp is itself a conditional publish on
  `e.ver`. If it succeeds, it clears them just as the write-back's own publish would. If its outcome is
  ambiguous, it is one more pending publish. It adds nothing to the bound.
- Keeping only the newest 8. An older one landing is the same false stale.

## 2. Never reuse a tried generation number, across incarnations

`lastGen` starts at the manifest's generation on every activation: after a restart, a discard, or a
passivation. Numbers a previous activation of the key tried and left as orphans are at the same epoch, so
their object names collide. The upload loop gave up after 8 create-only PUTs, each one a full upload (input
`80281010101010101010a0208010`; also `043b1e2902c6c8f8`).

Now, on `ErrExists` for an object that is not ours, `skipTaken` HEADs `gen+1, gen+2, gen+4, …`
(`StatGeneration`) until one is missing. `lastGen` jumps there, so one more upload follows. Numbers may be
skipped, which is fine: generations need only rise. Retention works from the names it saw published.

Why HEAD probes:

- LIST is off-limits on the agent's request path, and its credential has no LIST.
- Persisting `lastGen` would add a durable local write per upload, and it would still miss a lost VM's tries.
- Doing it in the reopen stamp would not cover discards and passivations.

Create-only PUTs keep the collision safe either way. This change is only about liveness and cost.

The deployment requirement: on AWS, a HEAD of a missing object answers 403, not 404, unless the identity has
`s3:ListBucket`. With 403 the HEAD proves nothing, so `skipTaken` falls back to probing with the create-only
PUT itself at doubling strides. That is safe, and 8 tries cover 255 taken numbers, but every probe is a full
upload. Grant `s3:ListBucket` on the key prefix; the agent still never calls LIST. Test: `durabletest`
`SetDenyMissingHead`.

## Tests

- `TestPendingPublishesInDoubtAreKeptUntilGrace`: 8 in flight → `ErrInDoubt`; the oldest lands and is adopted;
  8 failed → `ErrInDoubt` until the grace window passes, then the write-back goes through.
- `TestUploadSkipsGenerationsTakenByOrphans`: 40 orphans after a restart; one write-back publishes above them.
- `FuzzDurableHostModel`: the model keeps each pending publish's send time and the grace window, and every
  in-flight publish may land. The three inputs above are in the corpus.
