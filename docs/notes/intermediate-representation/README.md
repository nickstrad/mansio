# ir

The intermediate representation (IR) protocol: `/v1/ops` requests (queries and atomic mutations with
preconditions) that the SDK sends and any backend executes. Spec:
[`intermediate-representation/spec/v1.md`](../../../intermediate-representation/spec/v1.md); plan:
`docs/plans/completed/intermediate-representation-protocol.md` (removed; see git history); vectors:
[`intermediate-representation/vectors/`](../../../intermediate-representation/vectors/README.md). Code:
`core/intermediate-representation/` (decode, canonical JSON, wire errors, execution rules),
`platform/ir/executor.ts` (the reference executor on SQLite), `sdk/` (the client: IR-3).

Scripts: `scripts/dev/deno-linux.sh '<deno command>'` runs SQLite-backed tests from an Apple-silicon Mac;
`scripts/dev/e2e-linux.sh deno|go` runs the e2e suite against either backend.

## Entries (newest first)

- [2026-11-10 read path](2026-11-10-read-path.md): one snapshot per run, guarded finds, conflict answers with
  the current state, version-checked cached reads; IR revision 1.1 (`seq`, table asserts); the traps.
- [2026-11-06 IR-5a multi-node](2026-11-06-ir-5a-multinode.md): controld authenticates legacy keys and
  sessions at resolve, create caps, the edge on `/v1/ops`, durable single-node tenancy, e2e `scale_out`, and
  the Jepsen-style run (kill -9 loses the write-back window by design: checked per crash-free segment).
- [2026-11-06 IR-4a tenancy and auth](2026-11-06-ir-4a-tenancy-auth.md): credentials on agentd's `/v1/ops`,
  the admin and sign-in API, Better Auth scrypt, the Postgres import, and the e2e helpers.
- [2026-11-06 IR-4c observability](2026-11-06-ir-4c-observability.md): agentd's JSONL event log (same envelope
  and redaction), the offline TS ingest and catalog over it, `X-Mansio-Handler`, k6 on `/v1/ops`.
- [2026-11-06 IR-4b durability](2026-11-06-ir-4b-durability.md): backup = forced write-back answering the
  manifest, `dbtool restore -gen`, `dbtool import` of Deno-written files, and `backup_restore` on Go.
- [2026-11-05 IR-3 SDK](2026-11-05-ir-3-sdk.md): the recording executor, derived preconditions and handler
  retry, the `/v1/ops` transport, the Deno shim route, and running e2e on Deno and Go.
- [2026-11-05 IR-0 reference executor](2026-11-05-ir-0-reference-executor.md): how the reference executor
  differs from today's mutation path, where the protocol's open questions were settled, and why SQLite tests
  need Linux on Apple silicon.
