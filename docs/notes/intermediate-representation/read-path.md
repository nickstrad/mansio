# The read path: one snapshot, guarded finds, conflict state, cached reads

How one handler run reads and commits, what IR revisions 1.1 (`seq`, table asserts) and 1.3 (the sequence
after a server restore) added, and the traps. Spec:
[`intermediate-representation/spec/v1.md`](../../../intermediate-representation/spec/v1.md) §5.5, §6.5, §8.1,
§12, §14. What may come next:
[`docs/plans/colocated-runner-explainer.md`](../../plans/colocated-runner-explainer.md).

## What a run does

1. `client.query` and `client.mutation` share one loop (`sdk/client.ts`, `call`). Each iteration is a
   `Recording` (`sdk/recording.ts`) built from `RecordingOptions`: `reads`, `ids`, `now`, `writable`, `seed`,
   `cache`.
2. A `get` of a document the run does not know: a mutation asks the client's cache first (`sdk/cache.ts`); a
   hit sets `entry.unverified`. Otherwise the read is an Effect request, batched with the others asked for
   under `batching: true` (`db.getMany` does that), and sent by `Recording.fetch`.
3. `fetch` puts the run's `preconditions()` in front of the reads. The answer's `seq` becomes
   `Recording.snapshot`: everything read so far holds at it. A failed assert dies with `Restart(error, seed)`.
4. A `find` registers its table in `Recording.tables` at the sequence it was answered at; `preconditions()`
   emits `{op: "assert", table, expect: {unchangedSince}}` after the document asserts.
5. On return the client settles the run's outcome first. No commit to send (the handler failed, wrote nothing,
   or needs more than 64 ops) and `recording.pending`: `recording.verify()` sends the asserts alone, as a
   query. With writes: one mutation, asserts first.
6. On a derived conflict (commit or read), `Recording.rebase` reads `fields.seq` and `fields.current` and
   answers a `ReadSet`; the next run starts from it and reads only what it lacks.

Checked by running (2026-11-10): `tests/unit/sdk_read_path_test.ts` (20 tests, each counting requests), the
execute vectors, `scripts/dev/e2e-linux.sh`, and `make platform-up` (the smoke prints `makeMove`'s trips
through the edge: warm 1, cold 2, stale cache 2).

## Server side

- `_mansio_changes(table_name, seq)`: the row named `''` is the database's commit sequence, a row per table is
  that table's last change. `docexec.Init` creates it; `docexec.Check` reports whether it exists (one subquery
  on the schema-record read, no extra statement); the first mutation on an older file creates it
  (`server/internal/docexec/seq.go`).
- A request reads the table once (`run.sequence`), and a mutation that changed a row upserts the next sequence
  for `''` and each touched table in one statement (`run.commitSequence`).
- `witness.go` completes a `ConflictError` with `seq` and `current`, capped at 65 536 bytes.
- The answer's `seq` travels as `actor.Output.Value` (a `docexec.Answer`), not in the stored body, so a
  duplicate answer has none.

## Traps

- **Effect batches only under `batching: true`.** `Effect.all([...], { concurrency: "unbounded" })` alone
  sends one request per read. Checked: the test "reads asked for together are one round trip" failed until
  `batching: true` was added to the concurrent form.
- **Statement-counting tests.** `TestStatementFailureLeavesNoTrace` and the fuzz model
  (`internal/docexec/model_fuzz_test.go`) count the statements a request issues. The sequence read and write
  are two more, after the last operation, and belong to no operation.
- **Tests that pin response bodies.** `"seq":n` is now in every non-duplicate answer (`front/ops_test.go`,
  `front/credentials_test.go`, `cmd/agentd/ops_test.go`, `backup_test.go`).
- **An inserted document is not cached.** The client does not know the `_creationTime` the server stamped, so
  the first mutation on a document the same client just inserted is still two trips. A mutation answer that
  carried the request's clock reading would remove this (not implemented).
- **The find guard is the whole table.** Any change to the table fails it. `find(...).unguarded()` is the
  opt-out for display-only reads on busy tables.
- **A conflict without `current` must evict.** `rebase` drops every asserted document from the cache when the
  conflict does not say which are stale; otherwise the next run would read the same stale entries again.
- **Queries do not read the cache.** They fill it. A query has no commit to prove a cached read.

## Revision 1.3: the sequence after a server restore

The commit sequence had the `_version` flaw: a server that restored older state (kill -9 inside the RPO)
counted on from the restored number, so a `find`'s guard taken on the lost state held against a table with
other documents. Fixed (2026-11-11) in `server/internal/docexec/seq.go`; spec §5.5, §6.5, §14; the server side
is in [the version and sequence floor](../../../server/docs/notes/writeback/version-and-sequence-floor.md)
(last section). What it means for a client:

- `seq` only rises, but not by one: after a restore it jumps to the server's floor (2^32 per restart on a
  durable agent). Never compute a sequence; compare the ones answered.
- `unchangedSince: n` with `n` above the database's sequence is a conflict, also on a database that was never
  restored (execute vector "table assert: a sequence the database has not reached"). The conflict's `actual`
  and `current[].changed` can then be below `n`; `fields.seq` is where the database is.
- A guard taken before a server restart fails once after it, also when nothing was lost. The SDK needed no
  change: `Recording.rebase` drops the finds of every table listed in `current`, and the next run reads them
  again at `fields.seq`.
