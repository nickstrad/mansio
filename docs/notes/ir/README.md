# Intermediate representation

The intermediate representation (IR) protocol is the app wire: `POST /v1/ops` requests (queries and atomic
mutations with preconditions) that the SDK sends and the Go server executes. Spec:
[`ir/spec/v1.md`](../../../ir/spec/v1.md); shared vectors: [`ir/vectors/`](../../../ir/vectors/README.md).
Code: `core/ir/` (decode, canonical JSON, wire errors, execution rules), `sdk/` (the client that runs
handlers), and on the server `server/internal/docexec` (execution) and `server/internal/front` (the `/v1/ops`
route). The Go server runs every execute vector
([ADR 0013](../../adr/0013-go-server-replaces-deno-platform.md)).

- [The SDK runs handlers and talks `/v1/ops`](sdk-handlers.md): the recording executor, derived preconditions
  and handler retry, the transport's retry and routing rules, and running e2e on the Go server.
- [The read path](read-path.md): one snapshot per run, guarded finds, conflict answers with the current state,
  version-checked cached reads; IR revisions 1.1 (`seq`, table asserts) and 1.3 (the sequence after a
  restore); the traps.
- [Tenancy and auth on agentd](tenancy-and-auth.md): credentials on agentd's `/v1/ops`, the admin and sign-in
  API, Better Auth scrypt, the Postgres import, and the e2e helpers.
- [Backup, restore and import](backup-restore-import.md): backup = forced write-back answering the manifest,
  `dbtool restore -gen`, `dbtool import` of existing SQLite files, and the `backup_restore` scenario.
- [agentd's event log](event-log.md): the JSONL records agentd writes (envelope, redaction, drop rules),
  `X-Mansio-Handler`, and k6 on `/v1/ops`.
- [The IR on a cluster](multi-node.md): controld authenticates legacy keys and sessions at resolve, create
  caps, the edge on `/v1/ops`, durable single-node tenancy, the `scale_out` scenario, and the Jepsen-style run
  (kill -9 loses the write-back window by design: checked per crash-free segment).

## Running the tests

`scripts/dev/deno-linux.sh '<deno command>'` runs SQLite-backed Deno tests from an Apple-silicon Mac;
`scripts/dev/e2e-linux.sh` runs the e2e suite against the Go server.
