# 2026-11-05 — testing additions from the book guides (coordinator)

The book guides in `docs/books/` (summarized in `docs/coding-style.md` §8) add these to the checklist in
[README.md](README.md). Everything else there still applies.

**Failures below the interface** (Release It H4; RI-13)
- Every adapter's contract suite runs three ways: against the fake, the real service, and the real service
  through `internal/faultnet` (TCP faults) or `internal/objstore/s3test` (HTTP/S3 faults).
- Minimum fault table: refused, accept-then-silence/blackhole, headers-then-stall, dribble, 5xx storm,
  applied-but-reply-lost, truncated/corrupt body, half-open after idle.

**Object storage** (object-storage guide §7, §11)
- Run `TestS3Compat` (X1–X12) against every S3 backend/version before use; record results in
  `docs/notes/writeback/<date>-s3-compat-<backend>.md`.
- Fault switches `ApplyThenStale`, `LoseUploadReply` (durabletest), `ApplyThenPreconditionFailed` (s3test).
- `FuzzDurableHostModel` carries the GC op and its invariant (never delete current, pending, or K retained).

**SQL** (SQL guide G-20…G-22; PG-11)
- Every `.sql` file has a golden-output regression test (`go test ./queries -update` to refresh; review diffs).
- Every schema constraint has an off-point test (a row that must be rejected).
- CAS and other contended paths get a test with many workers on the same key.

**Under load and over time** (Release It H5; RI-15, after CP-4)
- Fault modes also run during a frontbench/wbbench run; soak with idle hours before calling a component done.
