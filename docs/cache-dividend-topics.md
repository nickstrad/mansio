# cache-dividend topic proposals from a survey of mansio

Date: 2026-11-06. Author: Fable 5.1, branch `docs/cd-topics`. Status: **proposals, awaiting Nick's picks.**

cache-dividend episodes are ~2–2.5 minutes of narration over diagrams and code stills. They discuss a repo's
code and design and never run it. The house format for a feature episode is **capability → scenario →
mechanism one level down → design choice/tradeoff**. Every topic below is tethered to something this repo
contains today, with file paths, doc names or commit SHAs as evidence, and each one says whether the thing is
implemented or design-stage.

Code baseline: `main` at `f491c15` (2026-11-06). Since IR-6 (`952794c`, ADR 0013) the server is the Go server
in `server/`; the Deno platform is gone. One correction to the brief that produced this survey: the Go
multi-node path (controld, the edge, moves, attest-dead and restore) is **implemented and exercised** by the
`scale_out` e2e scenario, the Jepsen-style run and the cluster simulation (IR-5a merge `9dc62fc`; DST-2/3
merge `339f6aa`). What stays design-stage is the `pages/` lineage of scale-out proposals (lanes, planes,
planes-fable) and the pieces explicitly marked unbuilt below (leases, WAL shipping). The S3 durability work is
implemented but **paused** by Nick on 2026-11-06 (`7cf4ba6`) with open issues; topics that touch it say so.

Calibration: the series has shipped two litefleet episodes (a baby multitenant SQLite platform). mansio is the
grown-up sibling, so the topics below lean on what mansio does that litefleet does not: handlers in the
client, actors, epochs, tickets, generations, and the test rigs that found real bugs.

## Ranked topics

### 1. mansio: the server never runs your code

**Feature.** Application handlers run in the caller's process inside the TypeScript SDK. The SDK records what
a handler read, buffers what it wrote, and sends the server one atomic mutation whose first operations are
preconditions derived from the reads. The Go server validates, compiles and runs IR only.

**Evidence.** ADR 0009 (`docs/adr/0009-ir-is-the-app-wire.md`); `sdk/recording.ts` (header comment is the
whole design in 25 lines); `sdk/overlay.ts` (read-your-writes for `find`); `ir/spec/v1.md` §5.4 (`expect`) and
§8 (`ConflictError`); README "How a mutation runs"; knowledge note `docs/knowledge/ir/2026-11-05-ir-3-sdk.md`;
property suite `tests/property/sdk_test.ts` (derived asserts equal the gets; committed state equals the
handler's view; concurrent increments converge).

**Angle.**

- _Capability:_ write `makeMove` as ordinary TypeScript against a document API, and get serializable-looking
  behavior without the server ever loading a line of app code.
- _Scenario:_ two players submit a move at once. Both handlers `get` the game, see version 7, and each buffers
  a patch. Both mutations arrive with `assert games/<id> version 7` as op 0. The second one fails
  `ConflictError` on op 0, the SDK re-runs its handler, it now reads version 8 and either wins or raises the
  app's own conflict.
- _Mechanism:_ `Recording` turns every `get` into a guarded entry; at return it emits
  `[assert…] ++ [insert|patch|delete…]` as one `POST /v1/ops` mutation with a fresh `requestId`. A conflict
  with `op < asserts` is "ours" (re-run, bounded, jittered); any other conflict belongs to the app and
  surfaces to the caller. `find` is deliberately unguarded in v1.
- _Design choice:_ handlers must be side-effect-free beyond `db` because they can run more than once; the cost
  is paid in the client instead of embedding a TypeScript runtime in the server. The reward is that any
  backend passing `ir/vectors/` can serve any app.

**Why it works on screen.** One split frame: handler source on the left (`app/functions.ts` `makeMove`), the
JSON mutation it becomes on the right with the derived `assert` highlighted. Then a two-lane timeline of the
race, with the loser's re-run drawn as a loop. The 25-line header of `sdk/recording.ts` is a legitimate code
still on its own.

**Caveat.** Implemented and in the code today. Mention honestly that `find` reads are unguarded (v1.1 reserves
a range precondition).

### 2. mansio: one goroutine per database

**Feature.** Each SQLite database is served by exactly one goroutine with a bounded mailbox and one write
connection; there is no lock shared between databases. Queued writes commit as one transaction (group commit);
reads run after the commit, optionally on a WAL read pool. The driver is pure-Go SQLite, so a slow statement
holds a goroutine, not an OS thread.

**Evidence.** `server/internal/actor/actor.go` (package comment); `server/STATE.md` findings F1 and F2 (the
numbers: ~20k ok qps per process versus ~220 under one global lock on identical work; ~319k writes/s with
group commit versus 16.5k when every commit fsyncs; ~3 fds and ~0.7 MB per open database at 5k databases);
`docs/architecture/scaling_issues.md` (the Deno platform's one event loop and global locks, issues 2–5); ADR
0013 "Why" (this design is the reason the Deno platform was replaced).

**Angle.**

- _Capability:_ thousands of tenant databases in one process, where one tenant's slow query cannot stall
  another's.
- _Scenario:_ 1,000 databases, 1% of requests are a 200 ms runaway CTE. Under a single lock, every database
  waits behind it (p99 lock wait 1–1.8 s, ~2% success). With an actor per database the runaway hurts only its
  own mailbox.
- _Mechanism:_ `Submit` never blocks (full mailbox = `shed`); a batch is `BEGIN IMMEDIATE`, all queued writes,
  `COMMIT`, then reads; a statement past `statement_timeout` is interrupted via context and its batch-mates
  get `aborted`; a request still queued past its deadline is answered `expired` without running.
- _Design choice:_ within one database, one connection means head-of-line blocking; the read pool fixes that
  at the price of more file descriptors and more doomed slow reads running concurrently. Isolation per
  database was chosen over a shared pool because removing the shared lock "is the whole game".

**Why it works on screen.** The F1 comparison table is the hero still (three columns, three phases). A diagram
of mailboxes feeding actors feeding files, with one red runaway request in one lane. The `actor.go` package
comment reads as a code still.

**Caveat.** Implemented; this is agentd's execution core. The numbers come from the `sqlbench` PoC on an
Apple-silicon laptop, so present them as "same machine, same work" ratios, not absolute capacity claims.

### 3. mansio: epochs and fences, or how a database moves without two writers

**Feature.** Ownership of a database is an epoch handed out by an authority (placement rows in Postgres or a
file store). A process admits a request only if the epoch matches its durable local ACTIVE state. A move is
CAS to MOVING, fence the source (stop, drain, final write-back, durable FENCED), flip to the target at epoch+1
only with the source's acknowledgement, then the target claims and stamps storage with its epoch. Object
storage is the backstop: a lower epoch's publish fails, and the zombie fences itself.

**Evidence.** `server/internal/owner/state.go` (package comment: the three layers), `move.go` (the four-step
protocol), `table.go`; the written safety argument `server/knowledge/fencing/2026-11-05-safety-argument.md`
(S1 one admitter per epoch, S2 linear published history, S3 moves keep acknowledged writes, and the table of
assumptions); the restart re-stamp fix `server/knowledge/fencing/2026-11-06-reopen-restamp.md` (`98ea226`);
design origin in `docs/architecture/scale-out-direct-actors.md` §3.2–3.4; e2e
`tests/e2e/platform/scenarios/scale_out.ts` (writers keep going while a database moves vm-a → vm-b → vm-a).

**Angle.**

- _Capability:_ move a live database between machines with no lost acknowledged write and no moment where two
  processes both write it.
- _Scenario:_ a database moves while four clients are writing; later a VM is wrongly declared dead while it is
  only partitioned. The old owner keeps serving its epoch until its next publish fails, then fences itself.
- _Mechanism:_ the local transition table (`Closed`, `Active`, `Fencing`, `Fenced`), the in-flight counter
  that Fence waits on, the conditional manifest publish carrying the epoch, and the Stamp a new owner (or a
  restarted one) writes before admitting anything.
- _Design choice:_ **no clocks.** No leases, no TTLs in ownership; the price is that a partitioned live owner
  keeps serving until fenced or its publish fails. Leases are written up as an option, not built
  (`2026-11-05-leases-options.md`).

**Why it works on screen.** A four-frame move sequence (ACTIVE → MOVING → fence/ack → ACTIVE at E+1). A "what
would break it" still: the mutation test where removing `Stamp` forked the published history in 14 s. The
S1/S2/S3 list is three lines of narration.

**Caveat.** Implemented in `internal/owner` and exercised by e2e, fuzz (`FuzzOwnershipModel`) and the cluster
simulation. The storage half depends on the paused S3 write-back (topic 5), so say that moves and fencing
stand on their own while the durability window is still an open decision.

### 4. mansio: the 200-byte ticket that takes the control plane off the query path

**Feature.** A client asks controld once "where is database D of tenant T?" and receives an endpoint plus an
Ed25519-signed ticket naming the tenant, database, owner VM, epoch, generation, principal, permissions and
expiry. The agent verifies the ticket offline with no network call. A move bumps the epoch, so every
outstanding ticket dies the moment the target activates, with no revocation traffic.

**Evidence.** `server/internal/ticket/ticket.go` (package comment: the binary encoding and why not JWT),
`signing.go` (key rotation lifecycle: published → signing → retired → removed), `keys.go` (lock-free key
ring), `cache.go` (a verified ticket skips only the ~25 µs signature check); ADR 0012 (single node checks
credentials itself; multi-node authenticates at resolve); `sdk/transport.ts` (`locatorRouter`: cache until
TTL, background refresh at ~80%, re-resolve on `wrong_owner`/`fenced` with `min_epoch`);
`docs/architecture/scale-out-direct-actors.md` §3.1–3.3 (why the locator may be stale); ticket golden shared
between Go and TS (`sdk/ticket.ts`, `tests/unit/sdk_client_test.ts`); rotation lifted into `ticket.Rotate` at
`44c3096`.

**Angle.**

- _Capability:_ a control-plane outage stops new resolutions after the ticket TTL, not running traffic.
- _Scenario:_ controld is stopped mid-run; twenty writes still succeed on cached routes and tickets (the
  `scale_out` e2e phase 2). Then the database moves; the next request gets 421 `wrong_owner`, the SDK
  re-resolves with `min_epoch = old + 1`, and carries on.
- _Mechanism:_ `version | kid | claims | signature`, signature over a context string plus every preceding
  byte; canonical decoding so one claim set has exactly one valid ticket; a key ring replaced atomically on
  rotation with an overlap at least one ticket lifetime long.
- _Design choice:_ correctness lives at the agent (epoch check against durable local state), load lives at the
  locator, and the two never need to agree in real time. That is what lets the locator be stale and
  horizontally scaled.

**Why it works on screen.** The ticket byte layout as a single annotated strip. A three-box diagram (client,
locator, agent) with the control plane visibly off the hot path. The key-rotation timeline is a clean
four-state strip.

**Caveat.** Implemented (IR-5a). Tickets expire for authentication only; they are not leases and do not bound
a zombie's reads, which is the honest hand-off to topic 3.

### 5. mansio: a database is a chain of immutable generations

**Feature.** In durable mode each database's cold copy is a sequence of immutable SQLite snapshots in S3 plus
one manifest naming the current generation. Uploads are create-only (`If-None-Match: *`); the manifest publish
is conditional on the version read and refused if a higher epoch owns it. A backup is "write back now and tell
me the generation"; a restore is "download that generation and verify its hash".

**Evidence.** `server/internal/durable/durable.go` (package comment: layout `dbs/<h2>/<key>/gen/<G>-e<E>.db` +
`manifest.json`, `ErrStale`, `ErrExists`); `server/internal/objstore/durable.go` (the conditional PUTs);
`server/internal/vactor/writeback.go`; backup/restore/import (`7ac4169`, `cb0c98a`,
`docs/knowledge/ir/2026-11-06-ir-4b-durability.md`); the pending-publish and generation-number fixes
(`806eae7`); GC and retention (`server/knowledge/writeback/2026-11-05-gc-and-gen-cache.md`); the pause note
`server/knowledge/writeback/2026-11-06-backup-paused-open-issues.md`.

**Angle.**

- _Capability:_ any agent can activate any database from S3, and a backup is nothing more than a pointer to a
  generation that already exists.
- _Scenario:_ a write-back races a backup call; the backup answers what is stored, which may be newer than
  what it uploaded. A crash between upload and publish leaves an orphan generation that is never reused and is
  swept later.
- _Mechanism:_ snapshot on a reader (the backup API, ~1 ms stall instead of a VACUUM INTO stall of hundreds of
  ms), sha256 and size in object metadata so a manifest can be rebuilt from the object alone, a publish that
  fails with 412 when anyone else moved the manifest, K = 2 retention plus an orphan sweeper.
- _Design choice/tradeoff:_ whole-file generations are simple and self-describing, and they are also why this
  is paused: writes are acknowledged before write-back, so a kill -9 can lose up to the window (about 11 s at
  shipped defaults), a 1 GB database written back every 30 s is ~2.8 TB of uploads a day, and incremental WAL
  shipping is designed but not built.

**Why it works on screen.** The bucket layout as a tree. A three-step strip: upload generation (create-only) →
publish manifest (conditional) → old owner's publish bounces with 412. The open-issues list is a strong
closing still because it is the repo's own honesty.

**Caveat.** Implemented and tested (e2e `backup_restore`, cluster sim), but **paused** on 2026-11-06 with Nick
planning to evaluate LiteFS or similar. The episode should frame it as "what was built, what it taught, why it
is on hold". WAL shipping (`2026-11-05-wal-shipping.md`) is design-only.

### 6. mansio: the TypeScript compiler is the oracle for Go

**Feature.** The document-to-SQL compiler exists twice: `core/compile/*` in TypeScript and
`server/internal/doccompile` in Go. The contract between them is data, not code: shared JSON vectors (decode,
validation, schema, compile, execute) and a generated differential corpus. Where they disagree, TS is right
until a vector says otherwise. Go must reproduce `JSON.stringify` of the schema byte for byte because the
schema fingerprint is part of every request.

**Evidence.** ADR 0010 (`docs/adr/0010-compiler-port-with-ts-oracle.md`); `ir/vectors/*.json`,
`ir/vectors/generated/compile-corpus.jsonl`; generators `scripts/ir/gen-vectors.ts`,
`scripts/ir/gen-compile-corpus.ts`; `tests/unit/ir_vectors_test.ts` (fails if a committed vector no longer
matches its generator); `work-scripts/bash/ir-differential.sh`; Go runners in `server/internal/doccompile`
(`TestSelectMatchesReference`) and `server/internal/docexec` (`TestExecuteVectors`); `GENERATED_SQL.txt` as
the human-readable canary; `ir/spec/v1.md` §2 (fingerprint) and §9 (canonical encoding, RFC 8785 numbers); the
2^53 number contract in `core/compile/select.ts:100` (`CAST(... AS REAL)`) pinned by
`tests/property/sqlite_oracle_test.ts` regressions 1a/1b.

**Angle.**

- _Capability:_ port a 550-line compiler to another language without the two silently drifting.
- _Scenario:_ a json-layout `number` at or beyond 2^53 is written by `JSON.stringify` as integer text and read
  back by `json_extract` as an exact INTEGER; filters and sort keys would disagree between layouts. The fix is
  a `CAST(... AS REAL)` in both compilers, pinned by a vector and a Go reference test whose number domain
  includes 2^53.
- _Mechanism:_ compile SQL and fingerprints in the vectors are golden output of `core/`; every other
  expectation is hand-written; every bug fix in either language adds a vector; a change to compile output is a
  protocol change.
- _Design choice:_ keep the slower-to-change oracle in the language that already had the tests, and make the
  port prove itself against data rather than against a shared library.

**Why it works on screen.** Two SQL strings side by side (TS output, Go output) with the one `CAST`
highlighted. A vector file excerpt. The "every bug fix adds a vector" rule is a one-line still.

**Caveat.** Implemented. The TS reference _executor_ was deleted at IR-6; `execute.json` runs only in Go now.
Say so if the episode mentions execute vectors.

### 7. mansio: request ids that travel with the file

**Feature.** Every mutation carries a `requestId`. The actor claims the id inside the write's own savepoint
(`insert … on conflict do nothing`); a claimed row means "new, apply", nothing inserted means "applied before"
and the statement never runs. The stored result is answered on a retry with `"duplicate": true`. Because the
table lives in the database file, dedupe survives a crash, a snapshot, a write-back and a move.

**Evidence.** `server/internal/actor/reqid.go` (the comment and DDL:
`_mansio_requests … strict, without
rowid`); `ir/spec/v1.md` §7 and the `expired` row of §8 (`ambiguous: true`
means a mutation may have committed, retry with the same id);
`server/knowledge/front/2026-11-05-request-ids.md`; `move.go` comment ("request-id de-duplication is not here:
it lives in the database"); SDK side in `sdk/transport.ts` (canonical body built once so a retried mutation
keeps its id) and `sdk/client.ts` (new id per handler re-run, same id per transport retry).

**Angle.**

- _Capability:_ at-most-once application of a mutation across retries, reroutes and owner changes.
- _Scenario:_ the reply to a committed `makeMove` is lost; the SDK retries with the same id; the retry lands
  on a _different_ VM after a move and is still answered from the record because the record moved with the
  file.
- _Mechanism:_ the claim reads `sqlite3_changes()` rather than `RETURNING` (measured ~1.7 µs cheaper per
  write); only applied writes leave an id row because a failed write rolls back its claim; retention is a
  bounded set-based sweep on the `applied_ms` index inside later batches; 24 h TTL.
- _Design choice:_ the payload is not compared, so the same id with a different body gets the first answer;
  clients must never reuse ids. Scope is one database, mutations only.

**Why it works on screen.** The 4-line DDL. A timeline with a lost reply, a retry arrow, and the
`duplicate: true` answer. A move drawn as the table riding inside the snapshot.

**Caveat.** Implemented. Pairs naturally with topic 8 if the series wants a two-parter.

### 8. mansio: what the Jepsen-style run found

**Feature.** A real cluster (controld, two durable agents, the edge, Postgres, SeaweedFS) under a nemesis
(proxy faults, kill -9 and restart, moves, pausing Postgres and S3), with six clients sending raw IR and a
Wing–Gong–Lowe linearizability checker over each database's history.

**Evidence.** `server/cmd/controld/jepsen_test.go` (`b46a8c3`); `server/internal/linearize/linearize.go`
(`4abee64`, package comment); `docs/knowledge/ir/2026-11-06-ir-5a-multinode.md` "Jepsen-style run";
`docs/plans/ir-protocol.md` §9 (the open decision, with the correction that the 170 ms figure was the test's
200 ms window and shipped defaults lose about 11 s); the checker's crash-segment fix `f56cb87`.

**Angle.**

- _Capability:_ check a document database's history for linearizability per database, with unknown-outcome
  operations and partitions.
- _Scenario:_ the first unsegmented runs failed: a register read 6954 after 6999 had been acknowledged and
  read. That was not a checker bug. A restarted agent serves from its last published generation, so
  acknowledged writes inside the write-back window are rolled back by design.
- _Mechanism:_ the checker splits each database's history at the crashes of its owner; within a segment the
  history must be linearizable; a later segment may start from any state some operation wrote before the
  crash, never from a value nobody wrote; conservation (the bank total) is checked across crashes; the run
  logs an estimate of acknowledged writes lost per crash (40, 12, 14, 35, 0 in one 2.5 minute run).
- _Design choice:_ the test was made to accept the RPO rather than hide it, and the result became a written
  open decision (accept and document, shorten the window, acknowledge after publish, or WAL shipping).

**Why it works on screen.** The topology diagram with the nemesis as a hand on the wires. The
history-with-segments picture (one line per database, cuts at crashes). The `Op{Call, Return, Input,
Output}`
type and the `Unknown` return sentinel are a clean code still.

**Caveat.** The run and checker are implemented (gated by `MANSIO_JEPSEN=1`). The finding is a real, open
design weakness, now compounded by the `_version` reuse issue found by the simulation (topic 9). This episode
is only honest if it says the durability work is paused.

### 9. mansio: replaying the whole cluster from one seed

**Feature.** Deterministic simulation testing: `Clock`, `Rand` and `IDGen` seams in every domain package; one
agent (DST-1) or a 2–4 agent cluster with the real controller, locator, edge and ticket rotation (DST-2) runs
inside a `testing/synctest` bubble, firing exactly one timer or one sim event per step, so the same seed
produces a byte-identical trace. A nightly swarm varies fault rates per seed and shrinks failures to minimal
op scripts committed as regression vectors (DST-3).

**Evidence.** `server/internal/sim/doc.go` (package comment); `server/internal/det`;
`server/knowledge/determinism/README.md` and the DST-1/DST-2/3 notes; `docs/plans/ir-protocol.md` §6.4;
commits `dd959cc` (DST-2), `000b322` (DST-3), `60cca67` (shrinking keeps the failure class);
`server/scripts/sim-swarm.sh`; found bugs: the false supersede after a lost publish reply (DST-1), the crashed
incarnation's in-flight publish landing after restart (`98ea226`), write-backs stopping while a fence was
pending (`1addbbb`), and the open `_version` reuse after rollback
(`server/internal/sim/testdata/open/version-reuse-after-rollback.json`, `2d9a173`).

**Angle.**

- _Capability:_ a cluster failure that reproduces exactly from an integer.
- _Scenario:_ swarm seed 2761 reports a bank total of 398 instead of 400. Shrinking cuts the nemesis script to
  a few ops; the vector is committed as an open finding; replaying it is one command.
- _Mechanism:_ `synctest.Wait` for quiescence, then `Clock.Step` fires one due timer; per-database random
  streams so a crash cannot perturb another database's draws; doors that let one request per actor per step so
  woken requests arrive in order; trace lines sorted by stream and hashed with SHA-256; real pure-Go SQLite
  stays in the loop; crash points at fsync boundaries.
- _Design choice:_ determinism was bought by banning `time.Now` and `context.WithTimeout` in simulated
  packages and threading a clock through every deadline, rather than by a deterministic hypervisor (listed as
  a later option).

**Why it works on screen.** The seam table (Clock / Rand / IDGen: real vs seeded). A trace excerpt with its
hash. The "one thing runnable per step" loop as a four-box flow. The open vector's JSON is a dramatic closing
still.

**Caveat.** Implemented as test infrastructure; no CI job runs the swarm yet. Not exercised: group commit,
replica lag, controld crashes, clock skew (the repo lists these).

### 10. mansio: one schema, two physical layouts

**Feature.** The same typed schema compiles to either a columns layout (one STRICT column per field, JSON text
for arrays, CHECK constraints for enums) or a json layout (`_id`, `_creationTime`, `_version`, one `doc TEXT`
with `json_valid`), with index declarations becoming JSON expression indexes in the second. The decoder
returns the same document shape either way, and the layout is part of the schema fingerprint.

**Evidence.** `GENERATED_SQL.txt` (both DDLs and both SELECTs for the same plan, produced by
`deno task explain`); `core/compile/ddl.ts`, `core/compile/select.ts` (per-dialect field access,
`CAST(... AS REAL)` for json-layout numbers); README "Two physical layouts, same logical schema"; the number
contract pinned by `tests/property/sqlite_oracle_test.ts` and Go's `TestSelectMatchesReference`; the sample
app ships both (`tic-tac-toe` columns, `tic-tac-toe-json`); knowledge note
`docs/knowledge/compiler/2026-11-05-table-lookup-and-compile-select.md`.

**Angle.**

- _Capability:_ pick row-per-document or column-per-field per database without changing a handler.
- _Scenario:_ `db.find("games", { status: "playing" }).sort("_creationTime", "desc").limit(20)` becomes
  `WHERE "status" = ?` in one layout and `WHERE json_extract("doc", '$.status') = ?` in the other, both with
  `_id` as a stable tie-breaker and explicit null ordering.
- _Mechanism:_ a dialect object selects field access; DDL emits STRICT tables and `CHECK (... IN (...))` enums
  in columns mode and `json_valid` plus expression indexes in json mode; values are always bind parameters;
  arrays are JSON text in both.
- _Design choice:_ sameness is enforced by the oracle vectors rather than by trusting the abstraction; the one
  known divergence (doubles at 2^53) is handled by a CAST on the json side so filters, sorts and `countBy`
  keys agree.

**Why it works on screen.** The two DDL blocks side by side are the whole episode's spine, then the two
SELECTs. Tiny, concrete, and a nice companion to the earlier litefleet and DuckDB-Parquet episodes.

**Caveat.** Implemented. Clearly scope the query language (equality, chained where, numeric gt, sort, limit,
count/countBy; no joins, no projection, no cursors) so the episode does not overclaim.

### 11. mansio: JSONL is the truth, DuckDB is the cache

**Feature.** agentd writes structured JSONL events (http, handler, tx, sql, queue, backup, lifecycle) under
`<dir>/server/<date>/`, byte-identical to the TypeScript record format. An offline CLI ingests them
exactly-once into an append-only DuckDB file, runs a fixed catalog of named read-only analyses, and sweeps
retention only after committed ingest. One trace id spans client, edge and agent.

**Evidence.** ADR 0004 (DuckDB opened only by the reader), ADR 0006 (event identity, schema v2), ADR 0007
(retention follows committed ingest); `docs/observability.md` (tables, catalog, worked examples);
`core/events.ts`, `core/eventlog/ddl.ts`, `core/eventlog/offsets.ts`; the Go writer `server/internal/eventlog`
(`5015ce6`, `7a96f82`) with JS-exact redaction and number formatting and goldens shared with TS (`4206e0c`,
`38bb38a`: O_EXCL file creation, lost-at-flush counting); the CLI `eventlog/` and `scripts/logs.ts`; ADR 0013
§4 (why ingest stayed in TypeScript).

**Angle.**

- _Capability:_ ask "what did this request do in SQL, and how long did each statement take?" across processes,
  from files you can grep.
- _Scenario:_ a k6 run and the agent each log; `perf:ingest` loads both sides and a catalog query joins client
  and server latency on the trace id (`tests/e2e/observability_test.ts`).
- _Mechanism:_ one file per role, process, hour and size roll; `_ingest_state` is the only mutable table;
  every typed row and its `platform_events` row share one `event_id`; rejected lines are kept as evidence; the
  sweep gzips or deletes only closed, fully ingested files.
- _Design choice:_ keep DuckDB derived and rebuildable so the server never depends on it, and keep the ingest
  in TypeScript at the Go cut-over because porting ~3k tested lines bought no user-visible change.

**Why it works on screen.** The three-box flow (JSONL tree → DuckDB → named queries) already drawn in
`docs/observability.md`. A JSONL line beside its DuckDB row. The ADR 0007 rule "retention follows committed
ingest" is one still.

**Caveat.** Implemented. The Deno-era reader process and `/admin/logs/*` routes are gone; the episode should
describe the offline CLI shape.

### 12. mansio: lanes, planes, and the day the gateway left the query path

**Feature.** The scale-out design lineage rendered at nickstrad.github.io/mansio: `mansio-current`, the lanes
proposal (statement lanes, a placement table, a forwarder, per-tenant files), the planes proposal DP-01
(control plane holds routing and placement; every VM runs an agent daemon as the last mile for file work; a
tenant owns 0..N databases), the Fable revision DP-02 (route grants M1, lane view M3, default database M4,
operator-attested VM-loss recovery M5, backup manifests M6), and option 4 (control plane as locator and ticket
issuer; clients talk to database actors directly).

**Evidence.** `pages/content/mansio-lanes-proposal/inventory.md`,
`pages/content/mansio-planes-proposal/inventory.md`, `pages/content/mansio-planes-fable/inventory.md`;
`docs/architecture/scale-out-comparison-fable.md` (the pick and the eight modifications);
`docs/architecture/scale-out-direct-actors.md` (option 4); `docs/architecture/scale-out-go-greenfield.md`;
`docs/architecture/scaling_issues.md` (why one file per tenant and one event loop cannot scale); the landed
shape in `docs/architecture/c4-container-platform.terminal.md` (redrawn for IR-6).

**Angle.**

- _Capability:_ choose the unit of placement, and everything else follows.
- _Scenario:_ a hot tenant. Under lanes, a tenant is one file, so its writes and heavy scans share one WAL and
  one writer lock. Under planes and option 4, a tenant owns many databases, so hot and cold data can live in
  separate files on separate VMs.
- _Mechanism:_ the deciding argument from the comparison: lanes is the special case of planes where every
  tenant owns exactly one database, and nothing in lanes expresses the general case without becoming planes.
  Then option 4's one change: take the gateway off the query path and hand the client a ticket.
- _Design choice:_ a specified, adversarially reviewed lifecycle (create, move, drop, fence, restart) over a
  lighter control plane; then import the lighter control plane anyway as a locator.

**Why it works on screen.** This is the most diagram-native topic: the Pages site is literally the evidence,
and three C4 container views in sequence tell the story. Two stills from the comparison doc's tables close it.

**Caveat.** **Design-stage** for the lanes, planes and planes-fable datasets (every new element is marked
`planned`; M1 and M5 still await Nick's word per the inventory). Option 4's shape is what the Go server
implements (controld, tickets, actors, the edge), so the episode must say which boxes are drawings and which
are code: the Pages datasets still show the pre-IR-6 Deno platform as "current" and may lag `main`.

## Top 3 recommendations

1. **Topic 1, "the server never runs your code."** It is the idea that makes mansio mansio, it is fully
   implemented, the race scenario is intuitive, and the handler-source-versus-IR split frame is the best
   single still in the repo. It also sets up every later episode's vocabulary (IR, `assert`, `requestId`).
2. **Topic 2, "one goroutine per database."** The ~20k versus ~220 qps table is a rare measured, same-machine
   comparison that explains a rewrite decision (ADR 0013) in one picture, and it extends the litefleet
   episodes naturally: here is what the grown-up version does about the shared lock.
3. **Topic 3, "epochs and fences."** The deepest design in the repo with a written safety argument, a
   four-step protocol that storyboards itself, and a "what would break it" mutation test for the ending. It is
   implemented, and its one open edge (no leases, no clocks) is an honest, interesting tradeoff rather than a
   weakness to skirt.

Topics 4 (tickets) and 7 (request ids) are the natural fourth and fifth: each is a self-contained contract
with a tiny code still. Topics 5, 8 and 9 are strong but depend on saying plainly that the S3 durability work
is paused; I would schedule them after Nick decides the LiteFS question so the episode can state the outcome
rather than the open question.
