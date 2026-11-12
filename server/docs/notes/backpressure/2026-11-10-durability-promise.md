# The durability promise, stated and made visible (B10 (a), 2026-11-10)

Part (a) only. Part (b), an optional `max_dirty_age` that refuses writes to a database whose unpublished
writes are too old, is **not implemented**: it is gated on the paused LiteFS / write-back decision
([open issues](../writeback/2026-11-06-backup-paused-open-issues.md)), because WAL shipping changes what
"dirty age" means, and it trades write availability for a bounded loss window, which is a product decision.

What a slow or unreachable object store does today: each write-back fails fast, the database stays warm and
dirty, nothing in the write path looks at dirty age or the breaker, and writes keep being acknowledged. The
unpublished window is then as long as the outage.

Done:

- The promise is written exactly in the root `README.md` ("Transaction and consistency contract") and
  `docs/guide.md`: the acknowledgement is the local commit; the window is about 11 s with the defaults (the 10 s
  `write_back_every`, up to one 1 s tick, the copy and upload), not 10; it has no upper bound while S3 is down.
- The breach is visible: `mansio_vactor_dirty` (B1) beside `mansio_vactor_oldest_dirty_age_seconds`.
- The alert the control-plane plan decided on ("alert on `oldest_dirty_age`", `docs/control-plane-plan.md` §4):
  `MansioUnpublishedWritesOld` in `deploy/prometheus/backpressure.rules.yml`, critical when the oldest
  unpublished write is older than 60 s for 2 min (healthy is about 11 s).
- `TestStoreOutageShowsAsGrowingDirtyAge` (`internal/vactor/maintain_test.go`) pins the behaviour the promise
  describes: with the store down for 11 s, 1 min, 10 min and 1 h, both databases stay dirty, their writes are
  acknowledged and local only, every write-back is counted as a failure, the dirty age grows with the outage,
  and the first tick after recovery publishes them.

The 11 s figure is the open-issues note's (issue 1), not measured again here.
