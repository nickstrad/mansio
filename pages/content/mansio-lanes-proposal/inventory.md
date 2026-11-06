# Conversion inventory: `mansio-lanes-proposal`

Title: "Proposed: statement lanes and tenant placement (scale-out)". This dataset proposes **only** the
unbuilt scale-out design: statement lanes, the placement table, the forwarder, the lane supervisor, the peer
node and WAL frames. The FFI execution layer (FFI-01 to FFI-08: binding, statement classifier, deadline and
HTTP 504) has landed on `main` and is part of `mansio-current`; here it appears as `existing`, never as the
proposal.

Bundle:
[`public/architectures/mansio-lanes-proposal.json`](../../public/architectures/mansio-lanes-proposal.json).
Source: the proposed context and container pairs in `docs/plans/ffi/` on branch `docs/ffi-c4` at
[`81d9936`](https://github.com/nickstrad/mansio/tree/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi)
(not merged to `main`; read with `git show`), corrected against `main` at `2d30330`, where the FFI execution
layer (FFI-01 to FFI-08) has landed. Converted by PG-01 (`opus-pg-01`) on 2026-10-15; completed by PG-02
(`opus-pg-02`) the same day.

Scope: the **context** and **container (data node with lanes)** views (PG-01), and since PG-02 the **lane
component** view, the README's four key flows as **dynamic** views (plus their named failure alternatives) and
one **illustrative deployment**. The landscape and the code view are in `mansio-current` (the proposal adds no
person or system, and no code-level elements are drawn for unbuilt code).

Statuses: lanes, the placement table, the forwarder, the lane supervisor, the peer node and WAL frames are
`planned` (alone, or as `plannedNotes` of a `mixed` element or relationship). Everything that exists on `main`
is `existing`. The bundle test (`tests/bundles.test.ts`) fails if any element or relationship id is missing
from this file.

## Sources

| Source id              | File                                                                                                                                                                                       | Revision  |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------- |
| `src-ctx-github`       | [docs/plans/ffi/c4-context-ffi.github.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/c4-context-ffi.github.md)                       | `81d9936` |
| `src-ctx-terminal`     | [docs/plans/ffi/c4-context-ffi.terminal.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/c4-context-ffi.terminal.md)                   | `81d9936` |
| `src-ctx-py`           | [docs/plans/ffi/diagrams/c4-context-ffi.py](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/diagrams/c4-context-ffi.py)                   | `81d9936` |
| `src-ctr-github`       | [docs/plans/ffi/c4-container-ffi.github.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/c4-container-ffi.github.md)                   | `81d9936` |
| `src-ctr-terminal`     | [docs/plans/ffi/c4-container-ffi.terminal.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/c4-container-ffi.terminal.md)               | `81d9936` |
| `src-ctr-py`           | [docs/plans/ffi/diagrams/c4-container-ffi.py](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/diagrams/c4-container-ffi.py)               | `81d9936` |
| `src-ffi-readme`       | [docs/plans/ffi/README.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/README.md)                                                     | `81d9936` |
| `src-ffi-ste`          | [docs/plans/ffi/architecture-ste100.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/architecture-ste100.md)                           | `81d9936` |
| `src-lang-switch`      | [docs/architecture/lang-switch-fable.md](https://github.com/nickstrad/mansio/blob/d97bcf8f145e94279b882665c70412eabdaba775/docs/architecture/lang-switch-fable.md)                         | `d97bcf8` |
| `src-ffi-plan`         | [docs/plans/ffi-implementation.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/plans/ffi-implementation.md)                                     | `2d30330` |
| `src-adr-0008`         | [docs/adr/0008-statement-execution-via-ffi.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/adr/0008-statement-execution-via-ffi.md)             | `2d30330` |
| `src-sqlite-ffi`       | [platform/sqlite_ffi/README.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/sqlite_ffi/README.md)                                           | `2d30330` |
| `src-classify`         | [platform/sqlite_ffi/classify.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/sqlite_ffi/classify.ts)                                       | `2d30330` |
| `src-current-platform` | [docs/architecture/c4-container-platform.github.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-container-platform.github.md)   | `2d30330` |
| `src-current-context`  | [docs/architecture/c4-context-document-db.github.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-context-document-db.github.md) | `2d30330` |
| `src-lane-github`      | [docs/plans/ffi/c4-component-ffi-lane.github.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/c4-component-ffi-lane.github.md)         | `81d9936` |
| `src-lane-terminal`    | [docs/plans/ffi/c4-component-ffi-lane.terminal.md](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/c4-component-ffi-lane.terminal.md)     | `81d9936` |
| `src-lane-py`          | [docs/plans/ffi/diagrams/c4-component-ffi-lane.py](https://github.com/nickstrad/mansio/blob/81d9936df084763c1577f37bd844b4ff00b95fdc/docs/plans/ffi/diagrams/c4-component-ffi-lane.py)     | `81d9936` |
| `src-sqlite-ts`        | [platform/sqlite.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/sqlite.ts)                                                                 | `2d30330` |
| `src-document-reader`  | [platform/document_reader.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/document_reader.ts)                                               | `2d30330` |

## System Context — proposed lanes and tenant placement → diagram `lanes-context`

Source: `docs/plans/ffi/c4-context-ffi.github.md` and `.terminal.md`.

### Elements

| Source node | Source box                                                                                                                       | Element id  | Status   | Documents            |
| ----------- | -------------------------------------------------------------------------------------------------------------------------------- | ----------- | -------- | -------------------- |
| `db`        | Document DB platform [Software system]: Deno API nodes; statement lanes with FFI to libsqlite3 (planned); DuckDB event-log child | `platform`  | mixed    | `doc-platform`       |
| `registry`  | Central registry and placement [Software system, part of the platform]; placement table (planned)                                | `registry`  | mixed    | `doc-registry`       |
| `developer` | App developer [Person]                                                                                                           | `developer` | existing | none (box text only) |
| `caller`    | Application caller [Person]                                                                                                      | `caller`    | existing | none (box text only) |
| `operator`  | Platform operator [Person]; placement moves (planned)                                                                            | `operator`  | mixed    | none (box text only) |
| `objects`   | Object storage [External system]; WAL frames (planned, phase two)                                                                | `objects`   | mixed    | `doc-objects`        |
| `analytics` | Analytics consumer [External system]                                                                                             | `analytics` | existing | none (box text only) |

### Relationships

| Source edge | From → to                                             | Relationship id | Status   | Planned notes                                                                      |
| ----------- | ----------------------------------------------------- | --------------- | -------- | ---------------------------------------------------------------------------------- |
| (1)         | App developer → Document DB platform                  | `ctx-1`         | existing | —                                                                                  |
| (2)         | Application caller → Document DB platform             | `ctx-2`         | mixed    | Forwarding: a node that does not own the tenant forwards the request to the owner. |
| (3)         | Platform operator → Document DB platform              | `ctx-3`         | mixed    | Placement-move operations.                                                         |
| (4)         | Document DB platform → Central registry and placement | `ctx-4`         | mixed    | Placement lookup and lease heartbeat.                                              |
| (5)         | Document DB platform → Object storage                 | `ctx-5`         | mixed    | WAL frames (phase two).                                                            |
| (6)         | Document DB platform → Analytics consumer             | `ctx-6`         | existing | —                                                                                  |

## Container — data node with statement lanes → diagram `lanes-containers`

Source: `docs/plans/ffi/c4-container-ffi.github.md` and `.terminal.md`.

### Elements

| Source node | Source box                                                                            | Element id   | Status   | Documents            |
| ----------- | ------------------------------------------------------------------------------------- | ------------ | -------- | -------------------- |
| `api`       | API process [Deno process]; placement router, forwarder and lane supervisor (planned) | `api`        | mixed    | `doc-api`            |
| `lanes`     | Statement lanes (planned) [Deno child processes]; platform/lane.ts (planned)          | `lanes`      | planned  | `doc-lanes`          |
| `files`     | Tenant documents [SQLite 3 WAL files]; writer handle 3 fds, read-only handle 2        | `files`      | existing | `doc-files`          |
| `reader`    | Reader process [Deno child process]                                                   | `reader`     | existing | none (box text only) |
| `client`    | Typed client SDK [Library in caller process]                                          | `client-sdk` | existing | none (box text only) |
| `pg`        | Central registry and placement [Postgres 17 process/store]; placement table (planned) | `registry`   | mixed    | `doc-registry`       |
| `peer`      | Peer data node (planned) [Deno process on another host]                               | `peer`       | planned  | `doc-peer`           |
| `objects`   | Object storage [External system / filesystem]; WAL frames (planned)                   | `objects`    | mixed    | `doc-objects`        |

### Relationships

| Source edge | From → to                                    | Relationship id                 | Status   | Planned notes                                                                                                                                     |
| ----------- | -------------------------------------------- | ------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| (1)         | Typed client SDK → API process               | `ctr-1`                         | existing | —                                                                                                                                                 |
| (2)         | API process → Central registry and placement | `ctr-2`                         | mixed    | Placement lookup and lease heartbeat.                                                                                                             |
| (3)         | API process → Peer data node                 | `ctr-3`                         | planned  | —                                                                                                                                                 |
| (4)         | API process → Statement lanes                | `ctr-4`                         | planned  | —                                                                                                                                                 |
| (5)         | Statement lanes → System libsqlite3          | folded into `ctr-6` and `lanes` | planned  | Source label: "Deno.dlopen symbols: blocking step; nonblocking step; sqlite3_interrupt (planned)" — now the `technology` of `ctr-6` (decision P2) |
| (6)         | Statement lanes → Tenant documents           | `ctr-6`                         | planned  | —                                                                                                                                                 |
| (7)         | API process → Tenant documents               | `ctr-7`                         | mixed    | Retired when the lanes land: the lanes then own every open of a tenant file.                                                                      |
| (8)         | API process → Reader process                 | `ctr-8`                         | existing | —                                                                                                                                                 |
| (9)         | API process → Object storage                 | `ctr-9`                         | mixed    | WAL frames (phase two).; Snapshots taken from a read-only connection.                                                                             |

### Source labels that changed

| Id         | Source text (81d9936)                                                                                                                                   | Bundle text                                                                                                                                                                      | Why                                                     |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------- |
| `ctr-6`    | System libsqlite3 (planned) → Tenant documents: "WAL reads and writes; BEGIN IMMEDIATE per mutation; VACUUM INTO from a read-only connection (planned)" | Statement lanes → Tenant documents, same label without "(planned)"; technology "Deno.dlopen symbols: blocking step; nonblocking step; sqlite3_interrupt" (source edge 5's label) | P2: libsqlite3 is a library inside the lane             |
| `ctr-7`    | "node:sqlite reads and serialized mutations (today; moves into the lanes)"                                                                              | "Reads and serialized mutations through the FFI binding (today; moves into the lanes)"; technology "Deno FFI to system libsqlite3"                                               | P1: FFI-04 (`51a5283`) replaced `node:sqlite` on `main` |
| `platform` | "Deno API nodes; statement lanes with FFI to libsqlite3 (planned); DuckDB event-log child"                                                              | "Deno API node that executes SQLite through Deno FFI to the system libsqlite3 (landed, FFI-01 to FFI-08); DuckDB event-log child" + planned notes for lanes and several nodes    | P1                                                      |
| `api`      | "HTTP auth and tenant context; placement router, forwarder and lane supervisor (planned)"                                                               | landed FFI execution added to the description; router, forwarder, supervisor and the end of file access as planned notes                                                         | P1                                                      |
| `files`    | "One file per tenant; writer handle 3 fds, read-only handle 2"                                                                                          | "One file per tenant"; descriptor counts moved to `doc-measurements`                                                                                                             | P5                                                      |
| all        | literal "(planned)" in names, paths and labels                                                                                                          | removed; `status`, dashed stroke and the "planned" card text carry it                                                                                                            | P6                                                      |

## Component — statement lane process → diagram `lanes-components`

Source: `docs/plans/ffi/c4-component-ffi-lane.github.md` and `.terminal.md` (node lines are the Mermaid
file's). Diagram documents: `doc-lanes-components`, `doc-measurements`. Boundary group `g-lane` (dashed: the
lane process is planned). Converted by PG-02.

### Elements

| Source node (line) | Source box                                                                       | Element id                                | Status   | Why                                                                                                                     | Documents              |
| ------------------ | -------------------------------------------------------------------------------- | ----------------------------------------- | -------- | ----------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| `ipc` (L13)        | Lane IPC endpoint (planned) [Component]; platform/lane.ts (planned)              | `lane-ipc`                                | planned  | planned in the source and still unbuilt                                                                                 | none (box text only)   |
| `classify` (L14)   | Statement classifier (planned) [Component]; platform/lane/classify.ts (planned)  | `lane-classifier`                         | existing | landed in FFI-05 (`platform/sqlite_ffi/classify.ts`), same rule (P13)                                                   | `doc-lane-classifier`  |
| `sched` (L15)      | Write-first scheduler (planned) [Component]; platform/lane/schedule.ts (planned) | `lane-scheduler`                          | mixed    | `Semaphore(1)`, `WRITER_QUEUE_CAP`, `BEGIN IMMEDIATE` exist in `Storage` (`platform/sqlite.ts` L147, L482, L691)        | `doc-lane-scheduler`   |
| `pool` (L16)       | Bounded statement pool (planned) [Component]; platform/lane/pool.ts (planned)    | `lane-pool`                               | planned  | planned in the source and still unbuilt                                                                                 | `doc-lane-pool`        |
| `watchdog` (L17)   | Deadline watchdog (planned) [Component]; platform/lane/deadline.ts (planned)     | `lane-watchdog`                           | mixed    | deadline, interrupt and grace detection exist (FFI-05; `ConnectionGate`, `platform/sqlite.ts` L317); escalation planned | `doc-lane-watchdog`    |
| `handles` (L18)    | Handle LRU / read pool (planned) [Component]; platform/lane/handles.ts (planned) | `lane-handles`                            | mixed    | 64-entry leased handle cache exists (`makeStorage`, `platform/sqlite.ts` L998 to L1074); read pool planned              | `doc-lane-handles`     |
| `reqid` (L19)      | Request-id store (planned) [Component]; platform/lane/request_ids.ts (planned)   | `lane-request-ids`                        | planned  | planned in the source and still unbuilt                                                                                 | `doc-lane-request-ids` |
| `ffi` (L20)        | FFI binding (planned) [Component]; platform/ffi/sqlite.ts (planned)              | `lane-ffi`                                | existing | landed in FFI-01 to FFI-04b (`platform/sqlite_ffi/`) (P13)                                                              | `doc-lane-ffi`         |
| `events` (L21)     | Logger / EventSink [Effect logger/service]; platform/logging/                    | `lane-logger`                             | existing | existing in the source too: reused unchanged                                                                            | `doc-lane-logger`      |
| `api` (L23)        | API process [Deno process]; platform/server.ts; lane supervisor (planned)        | `api`                                     | mixed    | PG-01 status; now also tagged `external` (P12)                                                                          | `doc-api`              |
| `lib` (L24)        | System libsqlite3 (planned) [Shared library]; libsqlite3.so.0                    | folded into `lane-ffi` and `lane-14` (P2) | —        | library, not a container (P2)                                                                                           | —                      |
| `files` (L25)      | Tenant documents [SQLite 3 WAL files]; TENANTS_DIR/tenantId.db                   | `files`                                   | existing | PG-01 status; now also tagged `external` (P12)                                                                          | `doc-files`            |
| `jsonl` (L26)      | Lane JSONL (planned) [Filesystem]; LOG_DIR/lane-N/date/ (planned)                | `lane-jsonl`                              | planned  | planned in the source and still unbuilt                                                                                 | none (box text only)   |

### Relationships

| Source edge (line) | From → to                                       | Relationship id       | Status   | Planned notes / why                                                                                                            |
| ------------------ | ----------------------------------------------- | --------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------ |
| (1) (L27)          | API process → Lane IPC endpoint                 | `lane-1`              | planned  | —                                                                                                                              |
| (2) (L28)          | Lane IPC endpoint → Statement classifier        | `lane-2`              | planned  | —                                                                                                                              |
| (3) (L29)          | Statement classifier → Write-first scheduler    | `lane-3`              | planned  | —                                                                                                                              |
| (4) (L30)          | Statement classifier → Bounded statement pool   | `lane-4`              | planned  | —                                                                                                                              |
| (5) (L31)          | Write-first scheduler → Handle LRU / read pool  | `lane-5`              | mixed    | Read-only handle leases. Today only writer handles are leased, from the tenant handle cache.                                   |
| (6) (L32)          | Bounded statement pool → Handle LRU / read pool | `lane-6`              | planned  | —                                                                                                                              |
| (7) (L33)          | Write-first scheduler → Request-id store        | `lane-7`              | planned  | —                                                                                                                              |
| (8) (L34)          | Write-first scheduler → FFI binding             | `lane-8`              | mixed    | On the lane's main thread. Today Storage and the document reader make the same blocking step on the API process's main thread. |
| (9) (L35)          | Bounded statement pool → FFI binding            | `lane-9`              | planned  | —                                                                                                                              |
| (10) (L36)         | Bounded statement pool → Deadline watchdog      | `lane-10`             | planned  | —                                                                                                                              |
| (11) (L37)         | Deadline watchdog → FFI binding                 | `lane-11`             | existing | the call exists on `main` (P13)                                                                                                |
| (12) (L38)         | Deadline watchdog → API process                 | `lane-12`             | planned  | —                                                                                                                              |
| (13) (L39)         | FFI binding → System libsqlite3                 | folded into `lane-14` | —        | Source label "dlopen symbols; pointers and buffers (planned)" is now the `technology` of `lane-14` (P2)                        |
| (14) (L40)         | FFI binding → Tenant documents                  | `lane-14`             | existing | the call exists on `main` (P13)                                                                                                |
| (15) (L41)         | Write-first scheduler → Logger / EventSink      | `lane-15`             | existing | the call exists on `main` (P13)                                                                                                |
| (16) (L42)         | Bounded statement pool → Logger / EventSink     | `lane-16`             | planned  | —                                                                                                                              |
| (17) (L43)         | Logger / EventSink → Lane JSONL                 | `lane-17`             | planned  | —                                                                                                                              |

### Source labels that changed

| Id                            | Source text (81d9936)                                                      | Bundle text                                                                                             | Why     |
| ----------------------------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- | ------- |
| `lane-8`                      | "Blocking step on the lane main thread (6 to 7 µs point read) (planned)"   | "Blocking step on the lane main thread"                                                                 | P5, P6  |
| `lane-9`                      | "Nonblocking step on a pool thread (44 to 69 µs; Promise) (planned)"       | "Nonblocking step on a pool thread (Promise)"                                                           | P5, P6  |
| `lane-11`                     | "sqlite3_interrupt(db) at the deadline (about 0.4 ms to return) (planned)" | "sqlite3_interrupt(db) at the deadline"                                                                 | P5, P6  |
| `lane-14`                     | libsqlite3 → Tenant documents: "WAL reads and writes (planned)"            | FFI binding → Tenant documents, same label; technology "dlopen symbols; pointers and buffers" (edge 13) | P2      |
| `lane-classifier`, `lane-ffi` | paths `platform/lane/classify.ts`, `platform/ffi/sqlite.ts` (planned)      | the landed paths, with the source's path named in the description                                       | P1, P13 |
| `lane-handles`                | "Writer handle per file (3 fds), pooled read-only handles (2 fds)"         | descriptor counts moved to `doc-measurements`                                                           | P5      |
| all                           | literal "(planned)" in names, paths and labels                             | removed; `status` carries it                                                                            | P6      |

## Dynamic flows (PG-02)

Source: the README's "Key flows" (`src-ffi-readme`, line numbers of the README at `81d9936`) and their STE
restatement (`src-ffi-ste`, section numbers). Each flow is one `dynamic` diagram; a failure branch the source
names is its own named scenario, never a branch claiming both outcomes (P14). Every interaction reuses a model
relationship; the `edges` display number is that relationship's number in its static view. `reverse` marks a
response only. Step ids are listed for traceability; the steps read top to bottom.

### `flow-point-read` — Flow: a point read inside a lane (proposed)

Source: README "A point read" (L45 to L55); STE §4 (L227). Document: `doc-flow-point-read`,
`doc-measurements`. Members: `api`, `lane-ipc`, `lane-classifier`, `lane-scheduler`, `lane-handles`,
`lane-ffi`, `files`.

| Step id        | Kind        | Relationship | Direction | Label                                                                                                | Source                                  |
| -------------- | ----------- | ------------ | --------- | ---------------------------------------------------------------------------------------------------- | --------------------------------------- |
| `point-read`   | sequence    | —            | —         | —                                                                                                    | the flow                                |
| `pr-request`   | interaction | `lane-1`     | forward   | Send the statement request: compiled SQL, binds, deadline and request id (this node owns the tenant) | README 1 (edge 1)                       |
| `pr-classify`  | interaction | `lane-2`     | forward   | Classify the statement by its plan shape                                                             | README 2 (edge 2)                       |
| `pr-bounded`   | interaction | `lane-3`     | forward   | Bounded plan (an indexed lookup of at most one row): take the synchronous path                       | README 2 (edge 3)                       |
| `pr-lease`     | interaction | `lane-5`     | forward   | Lease a read-only handle for the tenant file                                                         | README 3 (edge 5)                       |
| `pr-step`      | interaction | `lane-8`     | forward   | Run a blocking step on the lane's main thread                                                        | README 3 (edge 8)                       |
| `pr-read`      | interaction | `lane-14`    | forward   | Read the row from the tenant's WAL file                                                              | README 3 (edges 13, 14)                 |
| `pr-row`       | interaction | `lane-14`    | reverse   | Return the row                                                                                       | README 3 (response of 13, 14); STE §4.7 |
| `pr-step-done` | interaction | `lane-8`     | reverse   | Return the row to the scheduler                                                                      | README 3 (response of 8)                |
| `pr-finish`    | parallel    | —            | —         | —                                                                                                    | README 4 "and": no order given          |
| `pr-result`    | interaction | `lane-1`     | reverse   | Send the result back over IPC                                                                        | README 4 (1, reverse)                   |
| `pr-release`   | interaction | `lane-5`     | forward   | Return the handle to the pool                                                                        | README 4; STE §4.9                      |

### `flow-long-query-interrupt` — Flow: a long query stopped at its deadline (proposed)

Source: README "A long query with a deadline and interrupt" (L57 to L69); STE §5 (L244). Document:
`doc-flow-long-query-interrupt`, `doc-measurements`. Members: `api`, `lane-ipc`, `lane-classifier`,
`lane-scheduler`, `lane-pool`, `lane-handles`, `lane-watchdog`, `lane-ffi`, `files`.

| Step id             | Kind        | Relationship | Direction | Label                                                                                | Source                                         |
| ------------------- | ----------- | ------------ | --------- | ------------------------------------------------------------------------------------ | ---------------------------------------------- |
| `long-query`        | sequence    | —            | —         | —                                                                                    | the flow                                       |
| `lq-request`        | interaction | `lane-1`     | forward   | Send the statement request with its deadline                                         | README 1 (edge 1)                              |
| `lq-classify`       | interaction | `lane-2`     | forward   | Classify the statement by its plan shape                                             | README 1 (edge 2)                              |
| `lq-unbounded`      | interaction | `lane-4`     | forward   | Unbounded plan (a list, an aggregate or a user predicate): take the nonblocking path | README 1 (edge 4)                              |
| `lq-lease`          | interaction | `lane-6`     | forward   | The tenant is within its sub-cap: lease a read-only handle                           | README 2 (edge 6)                              |
| `lq-register`       | interaction | `lane-10`    | forward   | Register the statement's deadline                                                    | README 2 (edge 10)                             |
| `lq-meanwhile`      | parallel    | —            | —         | —                                                                                    | README 3: the main thread stays free; STE §5.8 |
| `lq-pool-thread`    | sequence    | —            | —         | —                                                                                    | README 3 and 4                                 |
| `lq-step`           | interaction | `lane-9`     | forward   | Start the nonblocking step on a pool thread                                          | README 3 (edge 9)                              |
| `lq-read`           | interaction | `lane-14`    | forward   | Read the tenant's WAL file                                                           | README 3 (edges 13, 14)                        |
| `lq-interrupt`      | interaction | `lane-11`    | forward   | At the deadline, call sqlite3_interrupt(db) from the main thread                     | README 4 (edge 11)                             |
| `lq-interrupted`    | interaction | `lane-9`     | reverse   | The step returns SQLITE_INTERRUPT                                                    | README 4 (SQLITE_INTERRUPT)                    |
| `lq-error`          | interaction | `lane-1`     | reverse   | Send a typed deadline error back over IPC                                            | README 4; STE §5.12 (typed deadline error)     |
| `lq-main-thread`    | sequence    | —            | —         | —                                                                                    | README 3 "keeps serving point reads and IPC"   |
| `lq-other-request`  | interaction | `lane-1`     | forward   | Meanwhile, a point read for another statement arrives                                | README 3; point-read flow 1                    |
| `lq-other-classify` | interaction | `lane-2`     | forward   | Classify it: bounded plan                                                            | point-read flow 2                              |
| `lq-other-bounded`  | interaction | `lane-3`     | forward   | Take the synchronous path                                                            | point-read flow 2 (edge 3)                     |
| `lq-other-step`     | interaction | `lane-8`     | forward   | Run its blocking step on the main thread, which the long step leaves free            | point-read flow 3 (edge 8)                     |
| `lq-other-result`   | interaction | `lane-1`     | reverse   | Send that result back over IPC                                                       | point-read flow 4                              |

### `flow-long-query-queue-full` — Flow: a long query refused at the tenant's cap (proposed)

Source: README long-query step 2 failure branch (L61 to L63); STE §5.4. Document:
`doc-flow-long-query-queue-full`. Members: `api`, `lane-ipc`, `lane-classifier`, `lane-pool`.

| Step id        | Kind        | Relationship | Direction | Label                                                                                     | Source                                              |
| -------------- | ----------- | ------------ | --------- | ----------------------------------------------------------------------------------------- | --------------------------------------------------- |
| `queue-full`   | sequence    | —            | —         | —                                                                                         | the alternative                                     |
| `qf-request`   | interaction | `lane-1`     | forward   | Send the statement request with its deadline                                              | README 1 (edge 1)                                   |
| `qf-classify`  | interaction | `lane-2`     | forward   | Classify the statement by its plan shape                                                  | README 1 (edge 2)                                   |
| `qf-unbounded` | interaction | `lane-4`     | forward   | Unbounded plan: take the nonblocking path                                                 | README 1 (edge 4)                                   |
| `qf-refuse`    | interaction | `lane-4`     | reverse   | The tenant has used its whole sub-cap: refuse with a typed QueueFull for this tenant only | README 2 "typed QueueFull for that tenant only (4)" |
| `qf-error`     | interaction | `lane-1`     | reverse   | Send the typed QueueFull back over IPC                                                    | README 2; STE §5.4                                  |

### `flow-forwarded-mutation` — Flow: a mutation forwarded to the owner node (proposed)

Source: README "A forwarded mutation" (L71 to L82), receiving node; STE §6 (L265). Document:
`doc-flow-forwarded-mutation`. Members: `client-sdk`, `api`, `registry`, `peer`.

| Step id              | Kind        | Relationship | Direction | Label                                                                                                                     | Source                                               |
| -------------------- | ----------- | ------------ | --------- | ------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `forwarded-mutation` | sequence    | —            | —         | —                                                                                                                         | the flow                                             |
| `fm-request`         | interaction | `ctr-1`      | forward   | Send the mutation with the bearer credential, to any node                                                                 | README 1 (edge 1)                                    |
| `fm-auth`            | interaction | `ctr-2`      | forward   | Authenticate the credential and derive the tenant from it                                                                 | README 1 (edge 2); STE §6.1, §6.2                    |
| `fm-forward`         | interaction | `ctr-3`      | forward   | The placement says another node owns the tenant: forward with the trusted tenant id, the placement epoch and a request id | README 2 (edge 3); STE §6.5, §6.6                    |
| `fm-owner-result`    | interaction | `ctr-3`      | reverse   | The owner's result                                                                                                        | response of README 2 (3, reverse)                    |
| `fm-response`        | interaction | `ctr-1`      | reverse   | Return the result; the client contract is unchanged                                                                       | README 2 "client contract is unchanged" (1, reverse) |

### `flow-forwarded-mutation-owner` — Flow: the forwarded mutation on the owner node (proposed)

Source: README forwarded mutation steps 3 and 4, owner node; STE §6.7, §6.10 to §6.12. Document:
`doc-flow-forwarded-mutation-owner`. Members: `api`, `lanes`, `files`.

| Step id        | Kind        | Relationship | Direction | Label                                                                                                                | Source                            |
| -------------- | ----------- | ------------ | --------- | -------------------------------------------------------------------------------------------------------------------- | --------------------------------- |
| `owner-commit` | sequence    | —            | —         | —                                                                                                                    | the flow                          |
| `ow-to-lane`   | interaction | `ctr-4`      | forward   | The lease and the epoch are valid: send the mutation and its request id to the tenant's lane                         | README 3 (edge 4)                 |
| `ow-write`     | interaction | `ctr-6`      | forward   | Take this tenant file's writer lock only; BEGIN IMMEDIATE; write the mutation, the request id and the result; COMMIT | README 4 (edges 5, 6)             |
| `ow-result`    | interaction | `ctr-4`      | reverse   | Return the committed result to the API process, which answers the forwarder                                          | response of README 3 (4, reverse) |

### `flow-forwarded-mutation-stale-epoch` — Flow: a forward refused for a stale epoch (proposed)

Source: README forwarded mutation step 3 failure branch; STE §6.8, §6.9. Document:
`doc-flow-forwarded-mutation-stale-epoch`. Members: `client-sdk`, `api`, `registry`, `peer`.

| Step id          | Kind        | Relationship | Direction | Label                                                                                          | Source                                                     |
| ---------------- | ----------- | ------------ | --------- | ---------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `stale-epoch`    | sequence    | —            | —         | —                                                                                              | the alternative                                            |
| `se-request`     | interaction | `ctr-1`      | forward   | Send the mutation with the bearer credential                                                   | README 1 (edge 1)                                          |
| `se-auth`        | interaction | `ctr-2`      | forward   | Authenticate the credential and derive the tenant from it                                      | README 1 (edge 2)                                          |
| `se-forward`     | interaction | `ctr-3`      | forward   | Forward to the owner the cached placement names, with its epoch and a request id               | README 2 (edge 3)                                          |
| `se-wrong-owner` | interaction | `ctr-3`      | reverse   | WrongOwner: the epoch is stale                                                                 | README 3 "A stale epoch answers WrongOwner"                |
| `se-refresh`     | interaction | `ctr-2`      | forward   | Refresh the tenant's placement once from the placement table                                   | README 3 "refreshes the placement once" (edge 2); STE §6.9 |
| `se-retry`       | interaction | `ctr-3`      | forward   | Forward again to the owner the new placement names, with the new epoch and the same request id | README 3 "and retries" (edge 3)                            |
| `se-result`      | interaction | `ctr-3`      | reverse   | The owner's result                                                                             | response (3, reverse)                                      |
| `se-response`    | interaction | `ctr-1`      | reverse   | Return the result                                                                              | response (1, reverse)                                      |

### `flow-forwarded-mutation-retry` — Flow: a retried forward answered from the request-id store (proposed)

Source: README forwarded mutation step 5; STE §6.13, §6.14. Document: `doc-flow-forwarded-mutation-retry`.
Members: `api`, `lanes`, `files`.

| Step id     | Kind        | Relationship | Direction | Label                                                                                                 | Source                                                          |
| ----------- | ----------- | ------------ | --------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| `retry`     | sequence    | —            | —         | —                                                                                                     | the alternative                                                 |
| `rt-retry`  | interaction | `ctr-4`      | forward   | The first forward timed out: the retry arrives with the same request id and goes to the tenant's lane | README 5 "the retry presents the same request id" (edge 4)      |
| `rt-lookup` | interaction | `ctr-6`      | forward   | Look up the request id: the first attempt committed it                                                | README 5; STE §6.14 (request-id store, in the tenant file: P15) |
| `rt-stored` | interaction | `ctr-6`      | reverse   | Return the stored result                                                                              | STE §6.14 "answers from the store"                              |
| `rt-result` | interaction | `ctr-4`      | reverse   | Answer with the stored result; the mutation does not run again                                        | README 5 "instead of running the handler again"                 |

### `flow-lane-kill-restart` — Flow: a lane killed and restarted after a stuck statement (proposed)

Source: README "A lane kill and restart" (L84 to L93); STE §7 (L289). Document: `doc-flow-lane-kill-restart`,
`doc-measurements`. Members: `lane-watchdog`, `api`, `lanes`, `client-sdk`.

| Step id         | Kind        | Relationship | Direction | Label                                                                                        | Source                                      |
| --------------- | ----------- | ------------ | --------- | -------------------------------------------------------------------------------------------- | ------------------------------------------- |
| `kill-restart`  | sequence    | —            | —         | —                                                                                            | the flow                                    |
| `kr-escalate`   | interaction | `lane-12`    | forward   | The interrupt did not return within the grace period: ask the supervisor to kill the lane    | README 1 (Level 3, edge 12)                 |
| `kr-kill`       | interaction | `ctr-4`      | forward   | Send SIGKILL to the lane                                                                     | README 1 (Level 2, edge 4)                  |
| `kr-after-exit` | parallel    | —            | —         | —                                                                                            | README 2 and 3: no order given between them |
| `kr-fail`       | interaction | `ctr-1`      | reverse   | Fail each request whose statement was in flight on that lane with a typed lane-restart error | README 2; STE §7.3                          |
| `kr-respawn`    | sequence    | —            | —         | —                                                                                            | README 3                                    |
| `kr-spawn`      | interaction | `ctr-4`      | forward   | Spawn a replacement lane with the same sticky tenant set                                     | README 3 (edge 4)                           |
| `kr-next`       | interaction | `ctr-4`      | forward   | Send the next statement request for those tenants to the replacement lane                    | README 3 "answers its first request"        |
| `kr-answer`     | interaction | `ctr-4`      | reverse   | The replacement lane answers                                                                 | README 3; timing in `doc-measurements`      |

## Deployment → diagram `lanes-deployment` (illustrative, PG-02)

No source draws a deployment; c4-sdk Milestone B asks for one illustrative two-node example. Environment
`illustrative`; every member and edge is `planned`; document `doc-deployment` says it is not a claim about the
actual droplet (P16). Each instance maps to the logical element it instantiates; each edge names the
container-view relationship it instantiates (`modelRelationshipId`) except the node-to-node forward.

| Element id          | Kind           | Instance of / technology                                | Parent              | Grounding                                                       |
| ------------------- | -------------- | ------------------------------------------------------- | ------------------- | --------------------------------------------------------------- |
| `dep-node-a`        | deploymentNode | Linux host (illustrative)                               | —                   | container view: data node boundary "one of N, placement-routed" |
| `dep-node-b`        | deploymentNode | Linux host (illustrative)                               | —                   | container view: data node boundary "one of N, placement-routed" |
| `dep-registry-host` | deploymentNode | Postgres 17 host (illustrative)                         | —                   | container view `pg`                                             |
| `dep-object-store`  | deploymentNode | S3-compatible service or local directory (illustrative) | —                   | container view `objects`                                        |
| `dep-a-api`         | instance       | `api`                                                   | `dep-node-a`        | container view element `api`                                    |
| `dep-a-lane-1`      | instance       | `lanes`                                                 | `dep-node-a`        | container view element `lanes`                                  |
| `dep-a-lane-2`      | instance       | `lanes`                                                 | `dep-node-a`        | container view element `lanes`                                  |
| `dep-a-files`       | instance       | `files`                                                 | `dep-node-a`        | container view element `files`                                  |
| `dep-a-reader`      | instance       | `reader`                                                | `dep-node-a`        | container view element `reader`                                 |
| `dep-b-api`         | instance       | `api`                                                   | `dep-node-b`        | container view element `api`                                    |
| `dep-b-lane-1`      | instance       | `lanes`                                                 | `dep-node-b`        | container view element `lanes`                                  |
| `dep-b-lane-2`      | instance       | `lanes`                                                 | `dep-node-b`        | container view element `lanes`                                  |
| `dep-b-files`       | instance       | `files`                                                 | `dep-node-b`        | container view element `files`                                  |
| `dep-b-reader`      | instance       | `reader`                                                | `dep-node-b`        | container view element `reader`                                 |
| `dep-registry`      | instance       | `registry`                                              | `dep-registry-host` | container view `pg`                                             |
| `dep-objects`       | instance       | `objects`                                               | `dep-object-store`  | container view `objects`                                        |

| Relationship id    | From → to                      | Model relationship             | Number |
| ------------------ | ------------------------------ | ------------------------------ | ------ |
| `dep-a-ipc-1`      | `dep-a-api` → `dep-a-lane-1`   | `ctr-4`                        | 1      |
| `dep-a-ipc-2`      | `dep-a-api` → `dep-a-lane-2`   | `ctr-4`                        | 2      |
| `dep-a-wal-1`      | `dep-a-lane-1` → `dep-a-files` | `ctr-6`                        | 3      |
| `dep-a-wal-2`      | `dep-a-lane-2` → `dep-a-files` | `ctr-6`                        | 4      |
| `dep-a-reader-ipc` | `dep-a-api` → `dep-a-reader`   | `ctr-8`                        | 5      |
| `dep-a-registry`   | `dep-a-api` → `dep-registry`   | `ctr-2`                        | 6      |
| `dep-a-objects`    | `dep-a-api` → `dep-objects`    | `ctr-9`                        | 7      |
| `dep-a-forward`    | `dep-a-api` → `dep-b-api`      | none: stands for `ctr-3` (P16) | 8      |
| `dep-b-ipc-1`      | `dep-b-api` → `dep-b-lane-1`   | `ctr-4`                        | 9      |
| `dep-b-ipc-2`      | `dep-b-api` → `dep-b-lane-2`   | `ctr-4`                        | 10     |
| `dep-b-wal-1`      | `dep-b-lane-1` → `dep-b-files` | `ctr-6`                        | 11     |
| `dep-b-wal-2`      | `dep-b-lane-2` → `dep-b-files` | `ctr-6`                        | 12     |
| `dep-b-reader-ipc` | `dep-b-api` → `dep-b-reader`   | `ctr-8`                        | 13     |
| `dep-b-registry`   | `dep-b-api` → `dep-registry`   | `ctr-2`                        | 14     |
| `dep-b-objects`    | `dep-b-api` → `dep-objects`    | `ctr-9`                        | 15     |
| `dep-b-forward`    | `dep-b-api` → `dep-a-api`      | none: stands for `ctr-3` (P16) | 16     |

## Mermaid and terminal discrepancies (review notes)

Compared on 2026-10-15: in both pairs the prose above the picture differs only in the render marker, and the
relationship table and runtime notes are byte-identical. The pictures carry the same boxes in the same order.

1. **Terminal pictures draw no arrows.** Relationships exist only in the numbered key (identical in both
   formats).
2. **Planned edges are not dashed in the context Mermaid.** Context edges 2, 3, 4 and 5 carry planned parts
   (forwarding, placement moves, placement lookup and lease heartbeat, WAL frames) but are drawn solid (`-->`)
   with no "(planned)" word; only the boxes say "(planned)". The bundle marks them `mixed` with planned notes,
   which renders them dashed.
3. **Mixed boxes are drawn solid.** `db`, `registry`, `api`, `operator` and `objects` contain "(planned)"
   parts but use solid borders in both formats (only wholly planned boxes are dashed). The bundle marks them
   `mixed`.
4. **The registry changes type between views.** `[Software system, part of the platform]` in the context and
   `[Postgres 17 process/store]`, drawn outside the data-node boundary with the `external` class, in the
   container view. One element `registry` (a software system, decision P4) serves both.
5. **Edge 9 hides its planned part.** "Snapshot verification/hash and publication; WAL frames" is solid in
   Mermaid although WAL frames are planned (the box says "WAL frames (planned)"). The bundle marks `ctr-9`
   `mixed`.
6. **Timing numbers sit only in prose, not on the drawn labels**, in both formats; the bundle keeps them out
   of every label as well (P5).
7. **Lane component pair (PG-02).** Prose and relationship table are byte-identical apart from the render
   marker; the terminal picture draws no arrows. Both draw the logger solid (existing) and every edge dashed,
   including 15 to 17 from or to the existing logger. Unlike the context and container pairs, the lane view's
   Mermaid edge labels do carry timings (8: 6 to 7 µs, 9: 44 to 69 µs, 11: about 0.4 ms); the bundle moves
   them to `doc-measurements` (P5).
8. **The lane view uses the `external` class for the API process and the tenant documents**, which are
   containers of the platform (as in the current server component view, D3 of `mansio-current`). The bundle
   tags them `external` (P12).
9. **README versus STE on in-flight mutations after a kill.** The README says they "are resolved on retry
   through the request-id store, never replayed blind"; the STE text (§7.4) adds that the API process makes
   that retry with the same request id. The kill flow states only what both say and links the retry flow.
10. **The README cites this node's edges 4 to 6 for the owner node.** In the forwarded mutation, steps 3 and 4
    run on the owner, but the container view draws one node. The bundle splits the flow: the receiving node's
    part (`flow-forwarded-mutation`) and the owner's part (`flow-forwarded-mutation-owner`), whose arrows
    belong to the owner's own containers; the inbound forward has no logical edge and is not drawn (P14).
11. **Statement request carries SQL; the landed classifier reads the IR.** Edge 1 sends compiled SQL and
    binds, but `classify` (FFI-05) never reads SQL text. Recorded in `doc-lane-classifier` as an open point of
    the proposal; nothing is invented to close it.

## Conversion decisions

- **P1. Landed FFI parts are `existing`.** The source (2026-10-12, baseline `e808983`) marks the FFI binding,
  the statement classifier and the deadline/interrupt path planned. They landed on `main` in FFI-01 to FFI-08
  (2026-10-14; plan
  [`docs/plans/ffi-implementation.md`](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/plans/ffi-implementation.md);
  binding at `platform/sqlite_ffi/`, not the source's `platform/ffi/sqlite.ts`). They run in the API process,
  not in lanes. So `api`, `platform` and `ctr-7` describe them as existing, and only their placement inside
  lanes stays planned. FFI plan §6 confirms lanes and placement were not built.
- **P2. `libsqlite3` is not a container.** The source's "System libsqlite3 (planned) [Shared library via Deno
  FFI, one per lane]" is an implementation dependency loaded into a process. It is folded into the
  `technology` of `api` (existing) and `lanes` (planned). Source edge 5 (lanes → libsqlite3) becomes the
  technology of `ctr-6`, and edge 6 (libsqlite3 → files) becomes `ctr-6` from `lanes`, numbered 6. No
  relationship has number 5; the container key explains the gap. The lane's internal FFI binding component is
  on the lane component view (`lane-ffi`, added by PG-02; its edges 13 and 14 fold the same way into
  `lane-14`); no code-level FFI classes are invented.
- **P3. Planned instantiation is recorded separately from code reuse.** `lanes` is `planned`; its document
  says the code it would run exists today in the API process. Existing logger or binding code does not make a
  lane exist.
- **P4. The registry is a software system, as the source context draws it.** It sits in the organisation
  boundary group `g-organisation` with the platform (the source's "Document DB platform" subgraph). The
  current dataset models the same Postgres as a container of the platform; the two datasets keep their own
  identities.
- **P5. Timing numbers live in one document with provenance.** `doc-measurements` lists every number the
  source quotes (lang-switch study §3, vm-1, 2026-10-12), with the study's limits, and states that a bounded
  plan is not a wall-clock bound. No label, name or description carries a measurement. The descriptor counts
  on the files box moved there as well.
- **P6. "(planned)" words become status.** The source writes "(planned)" into names, paths and labels (belt
  and braces for plain Mermaid). The bundle uses `status`, which the SDK renders as a dashed stroke plus the
  words "planned" or "Existing + planned changes" on every card and in the text alternative.
- **P7. The peer data node is a planned container.** The source draws it as one box "same containers as this
  node" outside the data-node boundary. It is a planned container of the platform outside group `g-data-node`;
  PG-02's illustrative deployment `lanes-deployment` draws two data nodes as instances instead (P16).
- **P8. Omissions follow the source.** k6, the tenant/auth CLI and the migration runner are omitted, as the
  source context says ("unchanged and omitted here for focus"). The observability path is unchanged except for
  one JSONL directory per lane (source README, "Not drawn").
- **P9. Statuses of mixed relationships.** `ctx-2` (forwarding), `ctx-3` (placement moves), `ctx-4` and
  `ctr-2` (placement lookup and lease heartbeat), `ctx-5` and `ctr-9` (WAL frames; `ctr-9` also snapshots from
  a read-only connection), `ctr-7` (retired when lanes land) are existing relationships with planned notes.
- **P10. Documents.** Both diagrams and every element with source prose (`platform`, `registry`, `objects`,
  `api`, `lanes`, `files`, `peer`) have plain and STE (ASD-STE100 Issue 9, `draft`) texts. The STE texts draw
  on the source's own STE restatement (`architecture-ste100.md`) and are not certified.
- **P11. Renamed from its first id, which carried an `ffi` prefix (Nick, 2026-10-14).** The old name read as
  if the FFI execution layer were still proposed, but FFI-01 to FFI-08 landed and `mansio-current` already
  draws them. The dataset id is now `mansio-lanes-proposal` and its two diagram ids are `lanes-context` and
  `lanes-containers` (were `ffi-context` and `ffi-containers`; old deep links to the proposed dataset are not
  redirected). Content, statuses, relationships and the side-by-side mechanics are unchanged. Kept on purpose:
  the bundle `revision` string (`docs-ffi-c4-81d9936-on-main-2d30330-pg01`), the source ids (`src-ffi-readme`,
  `src-ffi-plan`, ...) and the source paths, because they name real files and the branch `docs/ffi-c4` the
  diagrams were converted from; source titles now say "Source diagram ... (branch docs/ffi-c4)" instead of
  "FFI ...".
- **P12. Supporting containers on the lane component view are tagged `external`** (D3 of `mansio-current`,
  applied here). c4-sdk rejects an untagged container on a component view, so `api`, `files` and `lane-jsonl`
  carry the tag; `api` and `files` therefore render grey on the container view too, and its key says why.
  `lane-jsonl` is a new planned container (the source's "Lane JSONL", and the README's "one JSONL directory
  per lane").
- **P13. Statuses inside the lane describe code and calls; the lane process is recorded once.** Per the brief
  and P3, the landed binding and classifier are `existing` (code reused unchanged, like the source's own
  logger), parts that exist with planned duties are `mixed` with notes (scheduler, watchdog, handle cache),
  lane-only parts are `planned` (IPC endpoint, statement pool, request-id store). A relationship is `existing`
  only where the same call exists on `main` (11 interrupt at the deadline, 14 binding to files, 15 Storage's
  sql/tx/queue events), `mixed` where its label adds a planned part (5 read-only leases, 8 "lane main
  thread"), else `planned` (including 3 and 4: on `main` the reader and Storage ask the classifier; the lane
  design inverts that). That the whole lane does not exist is carried by the dashed `g-lane` boundary and the
  planned `lanes` container.
- **P14. Flow grammar.** One `sequence` per flow; `parallel` only where the source gives no order between
  things that both happen (handle release and IPC reply in the point read; the long step and a concurrent
  point read; error replies and the respawn after a kill). Alternatives the source names (QueueFull, stale
  epoch, retry from the request-id store, and the interrupt that does not return, which is the kill flow) are
  separate named diagrams linked from each other, never two outcomes in one tree. The forwarded mutation is
  split by node (discrepancy 10). Steps never carry timings; a step that finishes before its deadline is
  described in the document, not drawn.
- **P15. Inferences, labelled as such.** The request-id store lives in the tenant's SQLite file (it is written
  in the mutation's transaction; the source names no table). The watchdog-to-supervisor message has no
  transport in the source; its `technology` says so.
- **P16. The deployment is illustrative.** Two data nodes with two lanes each, a registry host and object
  storage; counts and host technology are hypothetical and the document says it is not the droplet. The
  node-to-node forward has no `modelRelationshipId`: the container view draws the other node as one `peer`
  box, while here both ends are instances of `api`. `ctr-7` (the API process's direct file access) is not
  instantiated because lanes retire it.
- **P17. Navigation.** `lanes-containers` drills into `lanes-components` through the `lanes` node and links
  the Level 2 flows and the deployment; `lanes-components` links the Level 3 flows and the kill flow; each
  flow links its alternatives and its parent view; the context links the deployment. The bundle `revision` is
  now `docs-ffi-c4-81d9936-on-main-2d30330-pg02`.
