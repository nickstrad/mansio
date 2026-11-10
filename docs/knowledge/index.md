# Project knowledge

Durable mansio knowledge for future agents and the human: decisions, traps, and terms that survive any single
session. **Start here at session/orientation time**, then open the page relevant to your task.

Markdown only, by human decision (2026-10-11): no RAG index, no `kb` CLI. Sibling projects (`vm-agent`,
`perpetual`) use the nickstrad/kb full-text store; mansio deliberately stays plain markdown so any agent can
read and edit entries with zero tooling.

## Pages

| Page                                                     | Contents                                                              |
| -------------------------------------------------------- | --------------------------------------------------------------------- |
| [decisions.md](decisions.md)                             | Architectural decisions with dates and rationale                      |
| [gotchas.md](gotchas.md)                                 | Environment and tooling traps that have bitten agents before          |
| [glossary.md](glossary.md)                               | Project terms: items, batches, side quests, review protocol           |
| [haiku-experiment-record.md](haiku-experiment-record.md) | Preserved 2026-10-10 verdicts from the retired Haiku experiment track |

## Topics

One folder per part of the system, one dated entry per thread that explained it. Rules:
[AGENTS.md](AGENTS.md).

- [compiler/](compiler/README.md): how plans and schemas become SQL (`core/compile/`), and which process runs
  it.
  - [2026-11-05 select-all walkthrough](compiler/2026-11-05-select-all-walkthrough.md): `db.find("games")`
    from SDK call → plan → SQL → read-only SQLite in the server process → decoded documents.
  - [2026-11-05 table lookup and compileSelect](compiler/2026-11-05-table-lookup-and-compile-select.md): check
    order, placeholder numbering, per-dialect field access, and refusals.
  - [2026-11-05 SQLite-only simplification](compiler/2026-11-05-sqlite-only-simplification.md): DuckDB app
    queries are dead code; the event log is what keeps DuckDB.
- [ir/](ir/README.md): the IR protocol (`/v1/ops`), its spec and vectors; the Go server runs every execute
  vector (the TS reference executor was deleted in IR-6,
  [ADR 0013](../adr/0013-go-server-replaces-deno-platform.md)).
  - [2026-11-10 read path](ir/2026-11-10-read-path.md): how a run reads and commits after survey changes 1–4
    (one snapshot, guarded finds, conflict state, cached reads), IR revision 1.1, and the traps.
  - [2026-11-06 IR-4a tenancy and auth](ir/2026-11-06-ir-4a-tenancy-auth.md): agentd's auth port, admin and
    sign-in API, Better Auth scrypt, `controld import-mansio`.
  - [2026-11-05 IR-3 SDK](ir/2026-11-05-ir-3-sdk.md): handlers run in the SDK, derived preconditions and
    handler retry, e2e on Go (historical: the Deno `/v1/ops` shim and `BACKEND=deno` went in IR-6).
  - [2026-11-05 IR-0 reference executor](ir/2026-11-05-ir-0-reference-executor.md) (historical; deleted in
    IR-6): what changes versus the old mutation path, wire-tag reuse, and running SQLite tests from Apple
    silicon.

- [scale-out/](scale-out/README.md): what limits mansio past one process and machine, and the options.
  - [2026-11-05 mansio-server comparison](scale-out/2026-11-05-mansio-server-comparison.md): scaling issues
    mapped to the Go actor PoC's measurements; handlers-in-transaction is the constraint.
  - [Go greenfield proposal](../architecture/scale-out-go-greenfield.md): option 4 on mansio-server's actors;
    gate is the handler engine.

## Where the project stands

- Backpressure fixes B1–B11 landed on `main` 2026-11-10 (merge `039b7ed`): per-queue metrics, the retry
  contract ([`docs/retry-contract.md`](../retry-contract.md)), the capacity table
  ([`server/docs/capacity.md`](../../server/docs/capacity.md)) and its alert rules. Notes per finding:
  [`server/knowledge/backpressure/`](../../server/knowledge/backpressure/README.md). Two items wait on the
  LiteFS / write-back decision (`STATE.md`).
- IR plan complete (`docs/plans/ir-protocol.md`): IR-0 to IR-6 and DST-0 to DST-3 landed; the Deno platform is
  deleted and the Go server (`server/`) is the only backend
  ([ADR 0013](../adr/0013-go-server-replaces-deno-platform.md)). S3 backup work is paused
  ([open issues](../../server/knowledge/writeback/2026-11-06-backup-paused-open-issues.md)).
- `docs/plans/ffi-implementation.md` (historical; the FFI layer was deleted with the platform in IR-6) — the
  FFI statement execution layer: FFI-01 to FFI-08, including FFI-04b, all landed on `main` (FFI-08 merge
  `0b82504`; the plan record landed at `2d30330`). Decision record:
  `docs/adr/0008-statement-execution-via-ffi.md`.
- `pages/` — the architecture diagrams site (PG-01 landed 2026-10-15 as `b789441`; PG-02 landed as `f52c817`,
  completing the proposed dataset and rendering all seven C4 kinds): current and proposed C4 views side by
  side, on an interim c4-sdk copy until PG-03 swapped it for the published `c4-sdk` 0.1.0 npm package. See
  `pages/README.md` and the decisions of 2026-10-15.
- `agent-work/COORDINATION.md` — the coordinator's live log: what landed, what is in review, what is building.
- `docs/plans/effect-port-and-observability.md` and `docs/plans/multitenancy.md` — the build plans.
- `/root/MUSE_HATCH_README.md` — vm-1 orientation (read once per host).

## Maintenance convention

This folder only works if agents keep it current. Short, enforceable rules:

1. **Read** `index.md` at session/orientation start, before planning new work.
2. **Write** when you learn something durable: after a landing, a review, or a debugging session that taught
   you something another agent would otherwise rediscover. Add it to the right page in the same flow — not
   "later".
3. **Date** new entries (`YYYY-MM-DD`). Undated knowledge rots silently.
4. **Link, don't duplicate.** Plans (`docs/plans/`), architecture (`docs/architecture/`), and findings
   (`docs/haiku-experiments/findings/`) are authoritative for their details; knowledge entries record the
   _why_ and the _trap_, with a link.
5. **Mark superseded entries** with a date and a pointer to the replacement instead of deleting history.
   Delete only what would actively mislead.
6. **Update this index** whenever you add, move, rename, or remove a page.
7. **Never** store credentials, tokens, private runtime data, or unreviewed generated dumps here.
8. Run `deno fmt --check docs/knowledge/` after edits; the repo-wide fmt baseline stays green.
