# Conversion inventory: `mansio-current`

Bundle: [`public/architectures/mansio-current.json`](../../public/architectures/mansio-current.json). Source:
the four audited C4 pairs in `docs/architecture/` on `main` at
[`2d30330`](https://github.com/nickstrad/mansio/tree/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture)
(their audit stamp is `0b82504`, the FFI-08 merge; `2d30330` changes no file under `docs/architecture/`).
Converted by PG-01 (`opus-pg-01`) on 2026-10-15; PG-02 (`opus-pg-02`) added the landscape and the code view
the same day (sections at the end). Status: every element and relationship is `existing`, as the sources state
("All boxes describe landed code").

How to read this file: each source box (Mermaid node id, in declaration order) maps to one model element id;
each numbered source relationship maps to one relationship id and keeps its number as `displayNumber`.
Relationship labels are the source text verbatim; the `technology` field is derived (see decision D5). The
bundle test (`tests/bundles.test.ts`) fails if any element or relationship id is missing from this file.

## Sources

| Source id                               | File at 2d30330                                                                                                                                                                                        |
| --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `src-context-github`                    | [docs/architecture/c4-context-document-db.github.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-context-document-db.github.md)             |
| `src-context-terminal`                  | [docs/architecture/c4-context-document-db.terminal.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-context-document-db.terminal.md)         |
| `src-context-py`                        | [docs/architecture/diagrams/c4-context-document-db.py](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/diagrams/c4-context-document-db.py)         |
| `src-containers-platform-github`        | [docs/architecture/c4-container-platform.github.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-container-platform.github.md)               |
| `src-containers-platform-terminal`      | [docs/architecture/c4-container-platform.terminal.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-container-platform.terminal.md)           |
| `src-containers-platform-py`            | [docs/architecture/diagrams/c4-container-platform.py](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/diagrams/c4-container-platform.py)           |
| `src-containers-observability-github`   | [docs/architecture/c4-container-observability.github.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-container-observability.github.md)     |
| `src-containers-observability-terminal` | [docs/architecture/c4-container-observability.terminal.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-container-observability.terminal.md) |
| `src-containers-observability-py`       | [docs/architecture/diagrams/c4-container-observability.py](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/diagrams/c4-container-observability.py) |
| `src-components-server-github`          | [docs/architecture/c4-component-server.github.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-component-server.github.md)                   |
| `src-components-server-terminal`        | [docs/architecture/c4-component-server.terminal.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/c4-component-server.terminal.md)               |
| `src-components-server-py`              | [docs/architecture/diagrams/c4-component-server.py](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/diagrams/c4-component-server.py)               |
| `src-arch-readme`                       | [docs/architecture/README.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/README.md)                                                           |
| `src-arch-agents`                       | [docs/architecture/AGENTS.md](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/docs/architecture/AGENTS.md)                                                           |
| `src-document-reader`                   | [platform/document_reader.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/document_reader.ts)                                                           |
| `src-classify`                          | [platform/sqlite_ffi/classify.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/sqlite_ffi/classify.ts)                                                   |
| `src-sqlite-ts`                         | [platform/sqlite.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/sqlite.ts)                                                                             |
| `src-sqlite-ffi-effect`                 | [platform/sqlite_ffi/effect.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/sqlite_ffi/effect.ts)                                                       |
| `src-sqlite-ffi-database`               | [platform/sqlite_ffi/database.ts](https://github.com/nickstrad/mansio/blob/2d30330ed2c0aa967fb737827e43a7cbb8714e59/platform/sqlite_ffi/database.ts)                                                   |

## System Context → diagram `context`

Source: `docs/architecture/c4-context-document-db.github.md` and `.terminal.md`. Diagram document:
`doc-context`. Boundary group `g-platform` ("Document DB platform").

### Elements

| Source node | Source box           | Element id  | Element documents    |
| ----------- | -------------------- | ----------- | -------------------- |
| `db`        | Document DB platform | `platform`  | `doc-platform`       |
| `developer` | App developer        | `developer` | none (box text only) |
| `caller`    | Application caller   | `caller`    | none (box text only) |
| `operator`  | Platform operator    | `operator`  | none (box text only) |
| `objects`   | Object storage       | `objects`   | none (box text only) |
| `analytics` | Analytics consumer   | `analytics` | none (box text only) |
| `k6`        | k6 load generator    | `k6`        | none (box text only) |

### Relationships

| Source edge | From → to                                 | Relationship id | Derived technology                    |
| ----------- | ----------------------------------------- | --------------- | ------------------------------------- |
| (1)         | App developer → Document DB platform      | `ctx-1`         | TypeScript                            |
| (2)         | Application caller → Document DB platform | `ctx-2`         | HTTP JSON                             |
| (3)         | Platform operator → Document DB platform  | `ctx-3`         | HTTP / CLI                            |
| (4)         | Document DB platform → Object storage     | `ctx-4`         | S3 HTTPS or local files               |
| (5)         | Document DB platform → Analytics consumer | `ctx-5`         | DuckDB SQL (named catalog or offline) |
| (6)         | k6 load generator → Document DB platform  | `ctx-6`         | HTTP                                  |

## Container — request path → diagram `containers-platform`

Source: `docs/architecture/c4-container-platform.github.md` and `.terminal.md`. Diagram document:
`doc-containers-platform`. Boundary group `g-platform-host` ("Platform host and central registry").

### Elements

| Source node | Source box                                    | Element id   | Element documents    |
| ----------- | --------------------------------------------- | ------------ | -------------------- |
| `server`    | Server process                                | `server`     | `doc-server`         |
| `reader`    | Reader process                                | `reader`     | `doc-reader`         |
| `files`     | Application documents                         | `files`      | `doc-files`          |
| `pg`        | Central registry                              | `registry`   | `doc-registry`       |
| `tenants`   | Tenant/auth CLI                               | `tenant-cli` | `doc-tenant-cli`     |
| `migration` | Migration runner                              | `migration`  | none (box text only) |
| `client`    | Typed client SDK [Library in caller process]  | `client-sdk` | none (box text only) |
| `objects`   | Object storage [External system / filesystem] | `objects`    | none (box text only) |

### Relationships

| Source edge | From → to                              | Relationship id | Derived technology            |
| ----------- | -------------------------------------- | --------------- | ----------------------------- |
| (1)         | Typed client SDK → Server process      | `plat-1`        | HTTP JSON                     |
| (2)         | Server process → Reader process        | `plat-2`        | JSONL IPC                     |
| (3)         | Server process → Application documents | `plat-3`        | Deno FFI to system libsqlite3 |
| (4)         | Server process → Central registry      | `plat-4`        | Postgres protocol             |
| (5)         | Tenant/auth CLI → Central registry     | `plat-5`        | Postgres protocol             |
| (6)         | Migration runner → Central registry    | `plat-6`        | Postgres protocol             |
| (7)         | Server process → Object storage        | `plat-7`        | S3 HTTPS or local files       |

## Container — observability path → diagram `containers-observability`

Source: `docs/architecture/c4-container-observability.github.md` and `.terminal.md`. Diagram document:
`doc-containers-observability`. Boundary group `g-observability` ("Platform observability").

### Elements

| Source node | Source box                           | Element id     | Element documents    |
| ----------- | ------------------------------------ | -------------- | -------------------- |
| `server`    | Server process                       | `server`       | `doc-server`         |
| `reader`    | Reader process                       | `reader`       | `doc-reader`         |
| `jsonl`     | Structured event sources             | `jsonl`        | `doc-jsonl`          |
| `duck`      | Derived event log                    | `eventlog`     | `doc-eventlog`       |
| `cli`       | Logs CLI                             | `logs-cli`     | `doc-logs-cli`       |
| `loader`    | k6 point loader                      | `k6-loader`    | `doc-k6-loader`      |
| `results`   | k6 artifacts                         | `k6-artifacts` | none (box text only) |
| `k6`        | k6 load generator [External process] | `k6`           | none (box text only) |

### Relationships

| Source edge | From → to                                 | Relationship id | Derived technology |
| ----------- | ----------------------------------------- | --------------- | ------------------ |
| (1)         | Server process → Structured event sources | `obs-1`         | JSONL file append  |
| (2)         | Reader process → Structured event sources | `obs-2`         | JSONL file append  |
| (3)         | Server process → Reader process           | `obs-3`         | JSONL IPC          |
| (4)         | Reader process → Derived event log        | `obs-4`         | DuckDB in-process  |
| (5)         | Reader process → Structured event sources | `obs-5`         | Filesystem         |
| (6)         | Logs CLI → Derived event log              | `obs-6`         | DuckDB in-process  |
| (7)         | Logs CLI → Structured event sources       | `obs-7`         | Filesystem         |
| (8)         | k6 load generator → k6 artifacts          | `obs-8`         | JSON files         |
| (9)         | k6 artifacts → k6 point loader            | `obs-9`         | Filesystem         |
| (10)        | k6 point loader → Derived event log       | `obs-10`        | DuckDB in-process  |

## Component — server process → diagram `components-server`

Source: `docs/architecture/c4-component-server.github.md` and `.terminal.md`. Diagram document:
`doc-components-server`. Boundary group `g-platformlive` ("Server process: PlatformLive").

### Elements

| Source node | Source box                                     | Element id        | Element documents       |
| ----------- | ---------------------------------------------- | ----------------- | ----------------------- |
| `http`      | HTTP API / auth                                | `http`            | `doc-c-http`            |
| `life`      | Server lifecycle                               | `lifecycle`       | `doc-c-lifecycle`       |
| `apps`      | AppRegistry / AppServices                      | `apps`            | `doc-c-apps`            |
| `tenant`    | TenantRegistry                                 | `tenant-registry` | `doc-c-tenant-registry` |
| `route`     | TenantStorageRouter                            | `storage-router`  | `doc-c-storage-router`  |
| `context`   | TenantContext                                  | `tenant-context`  | `doc-c-tenant-context`  |
| `storage`   | Storage                                        | `storage`         | `doc-c-storage`         |
| `backup`    | Backups / ObjectStore                          | `backups`         | `doc-c-backups`         |
| `rpc`       | ReaderClient / ReaderProcess                   | `reader-client`   | `doc-c-reader-client`   |
| `docread`   | DocumentReaderClient                           | `document-reader` | `doc-c-document-reader` |
| `ffi`       | FFI SQLite binding                             | `ffi-binding`     | `doc-c-ffi`             |
| `classify`  | Statement classifier                           | `classifier`      | `doc-c-classifier`      |
| `logging`   | Logger / EventSink                             | `logging`         | `doc-c-logging`         |
| `config`    | Config / IdGen                                 | `config`          | `doc-c-config`          |
| `pg`        | Central registry [Postgres process/store]      | `registry`        | `doc-registry`          |
| `files`     | Application files [SQLite 3 WAL]               | `files`           | `doc-files`             |
| `reader`    | Reader child [Deno process]                    | `reader`          | `doc-reader`            |
| `objects`   | Object storage [External system / filesystem]  | `objects`         | none (box text only)    |
| `jsonl`     | Server JSONL [Filesystem] LOG_DIR/server/date/ | `jsonl`           | `doc-jsonl`             |

### Relationships

| Source edge | From → to                                        | Relationship id | Derived technology       |
| ----------- | ------------------------------------------------ | --------------- | ------------------------ |
| (1)         | HTTP API / auth → AppRegistry / AppServices      | `comp-1`        | In-process Effect call   |
| (2)         | HTTP API / auth → TenantRegistry                 | `comp-2`        | In-process Effect call   |
| (3)         | HTTP API / auth → Central registry               | `comp-3`        | Postgres protocol        |
| (4)         | HTTP API / auth → TenantContext                  | `comp-4`        | In-process Effect call   |
| (5)         | TenantContext → Storage                          | `comp-5`        | In-process Effect call   |
| (6)         | AppRegistry / AppServices → TenantStorageRouter  | `comp-6`        | In-process Effect call   |
| (7)         | TenantStorageRouter → FFI SQLite binding         | `comp-7`        | In-process function call |
| (8)         | Storage → TenantStorageRouter                    | `comp-8`        | In-process Effect call   |
| (9)         | HTTP API / auth → Storage                        | `comp-9`        | In-process Effect call   |
| (10)        | HTTP API / auth → DocumentReaderClient           | `comp-10`       | In-process Effect call   |
| (11)        | DocumentReaderClient → AppRegistry / AppServices | `comp-11`       | In-process Effect call   |
| (12)        | DocumentReaderClient → FFI SQLite binding        | `comp-12`       | In-process function call |
| (13)        | DocumentReaderClient → Statement classifier      | `comp-13`       | In-process function call |
| (14)        | HTTP API / auth → ReaderClient / ReaderProcess   | `comp-14`       | In-process Effect call   |
| (15)        | Storage → FFI SQLite binding                     | `comp-15`       | In-process function call |
| (16)        | Storage → Statement classifier                   | `comp-16`       | In-process function call |
| (17)        | Server lifecycle → Backups / ObjectStore         | `comp-17`       | In-process Effect call   |
| (18)        | Backups / ObjectStore → Storage                  | `comp-18`       | In-process Effect call   |
| (19)        | Backups / ObjectStore → FFI SQLite binding       | `comp-19`       | In-process function call |
| (20)        | Backups / ObjectStore → Object storage           | `comp-20`       | S3 HTTPS or local files  |
| (21)        | TenantRegistry → Central registry                | `comp-21`       | Postgres protocol        |
| (22)        | Server lifecycle → ReaderClient / ReaderProcess  | `comp-22`       | In-process Effect call   |
| (23)        | ReaderClient / ReaderProcess → Reader process    | `comp-23`       | JSONL IPC                |
| (24)        | Storage → Logger / EventSink                     | `comp-24`       | Effect logger            |
| (25)        | DocumentReaderClient → Logger / EventSink        | `comp-25`       | Effect logger            |
| (26)        | HTTP API / auth → Logger / EventSink             | `comp-26`       | Effect logger            |
| (27)        | Server lifecycle → Logger / EventSink            | `comp-27`       | Effect logger            |
| (28)        | Backups / ObjectStore → Logger / EventSink       | `comp-28`       | Effect logger            |
| (29)        | Logger / EventSink → Structured event sources    | `comp-29`       | JSONL file append        |
| (30)        | Config / IdGen → AppRegistry / AppServices       | `comp-30`       | Effect Config layer      |
| (31)        | Config / IdGen → TenantRegistry                  | `comp-31`       | Effect Config layer      |
| (32)        | Config / IdGen → TenantStorageRouter             | `comp-32`       | Effect Config layer      |
| (33)        | Config / IdGen → Logger / EventSink              | `comp-33`       | Effect Config layer      |
| (34)        | Config / IdGen → FFI SQLite binding              | `comp-34`       | Effect Config layer      |
| (35)        | Config / IdGen → Storage                         | `comp-35`       | Effect Config layer      |
| (36)        | Config / IdGen → DocumentReaderClient            | `comp-36`       | Effect Config layer      |
| (37)        | AppRegistry / AppServices → FFI SQLite binding   | `comp-37`       | Deno.dlopen              |
| (38)        | FFI SQLite binding → Application documents       | `comp-38`       | SQLite C API             |

## System Landscape → diagram `landscape` (PG-02)

No source draws a landscape; c4-sdk Milestone B asks for one of the modelled people and systems, inventing no
running system. Members and edges are exactly the system context's (`context`, source
`c4-context-document-db`), with no system in focus: `developer`, `caller`, `operator`, `platform`, `objects`,
`analytics`, `k6`; relationships `ctx-1` to `ctx-6` keep their context numbers. Document: `doc-landscape`.
`platform` drills into `context`, whose parent is now the landscape. Decision D10.

## Code — platform/document_reader.ts → diagram `code-document-reader` (PG-02)

Source: `platform/document_reader.ts` on `main` at `2d30330` (unchanged through `b789441`), plus the imported
symbols' declarations. Scope: component `document-reader` (DocumentReaderClient), which now drills into this
view. Documents: `doc-code-document-reader`, `doc-code-query-sqlite`, `doc-code-make-client`. Decisions D11,
D12.

### Elements

| Element id                | Symbol                       | Kind              | Owner component   | Source lines                                                                     | Members                                                                                      |
| ------------------------- | ---------------------------- | ----------------- | ----------------- | -------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `dr-module`               | document_reader.ts           | module            | `document-reader` | L1 to L515 (constants L77, L112, L512)                                           | DOCUMENT_READ_CONCURRENCY_CAP, DocumentReaderClientLive, QUERY_ERROR_TAGS                    |
| `dr-make-client`          | makeDocumentReaderClient     | function          | `document-reader` | L411 to L510                                                                     | makeDocumentReaderClient, call, semaphore, pending                                           |
| `dr-query-sqlite`         | querySqlite                  | function          | `document-reader` | L143 to L409 (locals L168, L191, L195, L219, L234, L264, L305, L316, L327, L361) | querySqlite, envelope, poisoned, stuck, admit, runSelect, select, executeIn, begin, end, run |
| `dr-native`               | native                       | function          | `document-reader` | L79 to L84                                                                       | native                                                                                       |
| `dr-native-now`           | nativeNow                    | function          | `document-reader` | L86 to L102                                                                      | nativeNow                                                                                    |
| `dr-query-context`        | QueryContext                 | interface         | `document-reader` | L104 to L109                                                                     | active, operations, startNanos, queueWaitMs                                                  |
| `dr-document-query-error` | DocumentQueryError           | type              | `document-reader` | L124 to L125                                                                     | DocumentQueryError                                                                           |
| `dr-is-query-error`       | isQueryError                 | function          | `document-reader` | L127 to L132                                                                     | isQueryError                                                                                 |
| `dr-message-of`           | messageOf                    | function          | `document-reader` | L134 to L137                                                                     | messageOf                                                                                    |
| `dr-elapsed-ms`           | elapsedMs                    | function          | `document-reader` | L139 to L141                                                                     | elapsedMs                                                                                    |
| `dr-ext-sqlite-ffi`       | sqlite_ffi/mod.ts (imported) | module (external) | `ffi-binding`     | imports L19; `effect.ts` L76, L330, L444; `database.ts` L392                     | Database.open, prepareCached, allAsync, isSqliteFfiError                                     |
| `dr-ext-classify`         | classify.ts (imported)       | module (external) | `classifier`      | import L20; `classify.ts` L42, L56                                               | classify, pathOf                                                                             |
| `dr-ext-connection-gate`  | ConnectionGate               | class (external)  | `storage`         | import L21 to L29; `sqlite.ts` L317, L324, L333, L348                            | constructor, poisoned, blocking, nonblocking                                                 |

### Relationships

| Relationship id | From → to                                     | Kind        | Source lines                       |
| --------------- | --------------------------------------------- | ----------- | ---------------------------------- |
| `code-1` (1)    | document_reader.ts → makeDocumentReaderClient | dependency  | L512 to L515                       |
| `code-2` (2)    | makeDocumentReaderClient → document_reader.ts | dependency  | L417                               |
| `code-3` (3)    | makeDocumentReaderClient → querySqlite        | dependency  | L454                               |
| `code-4` (4)    | makeDocumentReaderClient → messageOf          | dependency  | L463                               |
| `code-5` (5)    | querySqlite → sqlite_ffi/mod.ts (imported)    | dependency  | L156, L240, L246, L401             |
| `code-6` (6)    | querySqlite → classify.ts (imported)          | dependency  | L236, L270                         |
| `code-7` (7)    | querySqlite → ConnectionGate                  | dependency  | L162, L237, L243, L319, L337, L379 |
| `code-8` (8)    | querySqlite → QueryContext                    | association | L219, L234, L316                   |
| `code-9` (9)    | querySqlite → native                          | dependency  | L154                               |
| `code-10` (10)  | querySqlite → nativeNow                       | dependency  | L240, L319, L338, L386             |
| `code-11` (11)  | querySqlite → isQueryError                    | dependency  | L367                               |
| `code-12` (12)  | querySqlite → elapsedMs                       | dependency  | L273, L355                         |
| `code-13` (13)  | querySqlite → messageOf                       | dependency  | L257, L375, L403                   |
| `code-14` (14)  | native → messageOf                            | dependency  | L82                                |
| `code-15` (15)  | nativeNow → messageOf                         | dependency  | L100                               |
| `code-16` (16)  | isQueryError → document_reader.ts             | dependency  | L131                               |
| `code-17` (17)  | isQueryError → DocumentQueryError             | association | L127                               |
| `code-18` (18)  | messageOf → sqlite_ffi/mod.ts (imported)      | dependency  | L135                               |

## Mermaid and terminal discrepancies (review notes)

Compared on 2026-10-15 for all four pairs: the title, the prose above the picture, the numbered relationship
table and the runtime notes are byte-identical between `*.github.md` and `*.terminal.md`; only the picture
differs. The pictures carry the same boxes in the same reading order. Differences that matter for conversion:

1. **Terminal pictures draw no arrows.** All four terminal renderings show boxes only; every relationship
   exists only in the numbered key. The Mermaid graphs draw every edge. The bundle follows the key (identical
   in both).
2. **Terminal word wrap splits identifiers.** The 72-column drawer breaks tokens mid-word:
   `LOG_DIR/role/date/events-*.j` / `sonl` (observability), `platform/sqlite_ffi/classify` / `.ts` and
   `sqlite.statementInterrup` / `tGraceMs` (component). The Mermaid labels hold the unbroken text, which the
   bundle uses.
3. **"External" means "outside this boundary" in the component Mermaid.** The component graph applies the
   `external` class to Central registry, Application files, Reader child and Server JSONL, which are platform
   containers, not external systems. The terminal picture draws them as ordinary boxes below the boundary. The
   bundle keeps them as platform containers (see D3).
4. **Box type wording varies across views of one element.** Object storage is `[External system]` in the
   context and `[External system / filesystem]` in the container and component views; k6 is
   `[External system]` in the context and `[External process]` in the observability view; the registry is
   `[Postgres 17 process/store]` at Level 2 and `[Postgres process/store]` at Level 3. One element per concept
   keeps the Level 2 wording (D1).
5. **No other discrepancy found:** element sets, edge numbering and planned markers (none) agree in every
   pair.

## Conversion decisions

- **D1. One element per running thing.** A container that appears in several views is one model element:
  `server` (platform and observability views), `reader` (platform, observability and component views), `files`
  ("Application documents" at Level 2, "Application files" at Level 3), `registry` ("Central registry" at both
  levels). Where two views describe the same element differently, its description joins both texts (`server`,
  `reader`, `objects`, `k6`).
- **D2. "Server JSONL" is the `jsonl` container.** The component view's
  `Server JSONL [Filesystem]
  LOG_DIR/server/date/` is the server-role slice of the observability view's
  "Structured event sources" (`LOG_DIR/role/date/...`). It maps to `jsonl`; the element document says so. No
  second store is invented.
- **D3. SDK rule: supporting containers on a component view must be tagged `external`.** c4-sdk rejects a
  non-component, non-person member of a component view without the `external` tag. The server component view
  shows four sibling platform containers (`registry`, `files`, `reader`, `jsonl`), so those four carry the
  tag. Tags are per element, so these four also render grey on the two container views, although they are
  inside the platform boundary. The container-view keys say so explicitly, and the boundary groups show
  membership. Reported as a c4-sdk hand-off (allow sibling containers of the scope's system on a component
  view without the tag).
- **D4. The typed client SDK is an external software system.** The source types it
  `[Library in caller process]`. The model has no "library in another system" kind, and the caller's
  application is not modelled, so `client-sdk` is a software system tagged `external` with technology "Library
  in caller process". It appears only on the request path view, as in the source.
- **D5. Labels verbatim, technology derived.** The source puts protocol and payload in one label. The bundle
  keeps that label verbatim and adds a short `technology` (the canvas shows it on a second line). Derived
  values: the protocol named in the label or in the Level 2 edge it refines (`Postgres protocol`, `JSONL IPC`,
  `S3 HTTPS or local
  files`, `Deno FFI to system libsqlite3`), and for in-process edges the call style:
  `In-process Effect call` between Effect services, `In-process function call` into the binding or the pure
  classifier, `Effect logger` into the logger, `Effect Config layer` from Config, `Deno.dlopen` for the
  library load (37), `SQLite C API` for file I/O (38). These are reading aids, not new claims.
- **D6. Boxes keep their file paths.** C4 conventions put the type on its own line and the path underneath.
  The type goes to `technology` (containers, components) and the path leads the description (`path — text`).
- **D7. Drill-down.** Context `platform` → `containers-platform` (detail); `containers-observability` is a
  related link from the context node and from the request-path view. Both container views' `server` →
  `components-server`. The component view's parent is the request-path view. There is no Level 4 view (the
  source draws none).
- **D8. Documents.** Each diagram has a document restating its source prose (paragraph above the picture,
  runtime notes, audit stamp). Elements get a document where the runtime notes say something specific about
  them; the other elements (people, external systems, migration runner, k6 artifacts) show their box text
  only. Plain text restates the source; STE text (ASD-STE100 Issue 9) is a `draft` with no reviewer, not a
  certification.
- **D9. Notation mirrored from the source key.** Each key lists the source's three Mermaid classes (person,
  external, planned) plus the boundary and the numbered arrow. Planned is listed for completeness although
  nothing here is planned.
- **D10. One shared landscape, here.** mansio builds one software system, so the landscape is the context's
  people and systems without a focus. The typed client SDK is left out (a library with no landscape-level
  relationship, D4). The proposed dataset adds no person or system (its separate "Central registry and
  placement" system is the same Postgres, P4 of that inventory), so it has no landscape of its own and shares
  this one. Adding the view added navigation only to existing diagrams: `context.parent` (the landscape) and
  the `document-reader` node's `detail` (the code view); no element, relationship, status or label of PG-01
  changed. The bundle `revision` is now `main-2d30330-pg02`.
- **D11. The code view is landed code only.** Every function, interface and type alias `document_reader.ts`
  declares is a symbol; the module symbol carries its three module-level constants as members. Local closures
  of `querySqlite` (declared `const name = (params): result =>`) are its private members, written
  `name(params):
  result`; return types appear only where the source writes them. Exported means `public`,
  everything else `private`. Three imported symbols that decide how a read runs (the FFI binding's entry
  points, the classifier and `ConnectionGate`) are tagged `external` under their owning components
  (`ffi-binding`, `classifier`, `storage`). Other imports (core compile and decode, events, errors) are not
  drawn. No lane or FFI class is invented.
- **D12. Code-view numbering is this conversion's.** No Markdown source draws Level 4, so edges are numbered 1
  to 18 in the order listed above, and relationship kinds are `dependency` (a call) or `association` (a type
  use). Each relationship cites the source lines that make the call. The grounding check (member signatures
  against the pinned files, whitespace-insensitive) passed for every member when PG-02 converted it.
