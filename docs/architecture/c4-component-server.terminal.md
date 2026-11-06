# Component — server process

> **Historical.** This diagram describes the Deno platform's server process, which was deleted at IR-6 (ADR
> 0013). It is kept because older plans link it; it is not redrawn for the Go agent. Current pictures: the
> context and container pairs in this directory.

Audited against landed main `643b706` on 2026-10-11 (item 27); updated for FFI-01 to FFI-05 at `f2bf1c9` on
2026-10-14; FFI-06 at `5c2a499` changes only the runtime note on `path`; re-audited at `aa26b27` on 2026-10-14
(FFI-07 touches no C4 surface; FFI-08 changes no element). Both formats are generated from
`diagrams/c4-component-server.py`; numbered relationships are identical. All boxes described landed code at
that point.

<!-- c4-render: diagrams/c4-component-server.py -->

```text
╭╌ Server process: PlatformLive ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╮
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ HTTP API / auth              │   │ Server lifecycle             │ ╎
╎  │ [Component]                  │   │ [Component]                  │ ╎
╎  │ platform/http/;              │   │ platform/server.ts           │ ╎
╎  │ platform/auth/;              │   │ Scoped listener, backup and  │ ╎
╎  │ core/errors.ts               │   │ ingest timers, reader-exit   │ ╎
╎  │ Named routes, traces,        │   │ watch                        │ ╎
╎  │ tenant/API-key/session       │   └──────────────────────────────┘ ╎
╎  │ authentication; toHttpStatus │                                    ╎
╎  │ maps StatementDeadlineError  │                                    ╎
╎  │ to 504                       │                                    ╎
╎  └──────────────────────────────┘                                    ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ AppRegistry / AppServices    │   │ TenantRegistry               │ ╎
╎  │ [Effect services]            │   │ [Effect service]             │ ╎
╎  │ platform/registry.ts;        │   │ platform/tenants/store.ts    │ ╎
╎  │ platform/layers.ts           │   │ Lazy Postgres registry;      │ ╎
╎  │ App schema/handlers; Storage │   │ bounded hashed-key TTL cache │ ╎
╎  │ and Backups stack per app    │   └──────────────────────────────┘ ╎
╎  └──────────────────────────────┘                                    ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ TenantStorageRouter          │   │ TenantContext                │ ╎
╎  │ [Effect service]             │   │ [Request-local Effect        │ ╎
╎  │ platform/tenants/routing.ts  │   │ service]                     │ ╎
╎  │ LocalFolderRouter per app;   │   │ platform/tenants/routing.ts  │ ╎
╎  │ validated locations          │   │ Trusted credential-derived   │ ╎
╎  └──────────────────────────────┘   │ tenant id; scoped            │ ╎
╎                                     │ continuation                 │ ╎
╎                                     └──────────────────────────────┘ ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ Storage                      │   │ Backups / ObjectStore        │ ╎
╎  │ [Effect service]             │   │ [Effect services]            │ ╎
╎  │ platform/sqlite.ts           │   │ platform/backup.ts;          │ ╎
╎  │ Scoped handles;              │   │ platform/object_store.ts     │ ╎
╎  │ serialization; rollback and  │   │ Snapshot/hash/manifest;      │ ╎
╎  │ context expiry;              │   │ local or signed S3           │ ╎
╎  │ ConnectionGate; unbounded    │   └──────────────────────────────┘ ╎
╎  │ reads under the statement    │                                    ╎
╎  │ deadline                     │                                    ╎
╎  └──────────────────────────────┘                                    ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ ReaderClient / ReaderProcess │   │ DocumentReaderClient         │ ╎
╎  │ [Effect services]            │   │ [Effect service]             │ ╎
╎  │ platform/ipc.ts;             │   │ platform/document_reader.ts; │ ╎
╎  │ platform/layers.ts           │   │ platform/sqlite.ts           │ ╎
╎  │ Scoped child for             │   │ (ConnectionGate)             │ ╎
╎  │ ingest/analysis; capped      │   │ In-process SQLite queries;   │ ╎
╎  │ queue, deadline, trace       │   │ read-only connection;        │ ╎
╎  └──────────────────────────────┘   │ concurrency cap 32;          │ ╎
╎                                     │ ConnectionGate; unbounded    │ ╎
╎                                     │ selects under the statement  │ ╎
╎                                     │ deadline                     │ ╎
╎                                     └──────────────────────────────┘ ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ FFI SQLite binding           │   │ Statement classifier         │ ╎
╎  │ [Library in server process]  │   │ [Pure function]              │ ╎
╎  │ platform/sqlite_ffi/         │   │ platform/sqlite_ffi/classify │ ╎
╎  │ System libsqlite3 via        │   │ .ts                          │ ╎
╎  │ Deno.dlopen; every server    │   │ classify(operation) from the │ ╎
╎  │ SQLite open goes through it  │   │ plan IR, never SQL text:     │ ╎
╎  │ (FFI-04), node:sqlite is not │   │ insert, patch, delete and a  │ ╎
╎  │ loaded. Every open applies   │   │ select with _id equality and │ ╎
╎  │ node:sqlite-compatible       │   │ no aggregate are bounded     │ ╎
╎  │ defaults (double-quoted      │   │ (blocking path); every other │ ╎
╎  │ strings off, foreign keys    │   │ select, count and countBy is │ ╎
╎  │ on); darwin/aarch64 refused. │   │ unbounded (nonblocking path) │ ╎
╎  │ Blocking path (main thread)  │   └──────────────────────────────┘ ╎
╎  │ for writes and point reads;  │                                    ╎
╎  │ nonblocking path             │                                    ╎
╎  │ (allAsync/getAsync on the    │                                    ╎
╎  │ blocking pool,               │                                    ╎
╎  │ sqlite3_interrupt at the     │                                    ╎
╎  │ deadline) for unbounded      │                                    ╎
╎  │ reads (FFI-05); 64-statement │                                    ╎
╎  │ cache                        │                                    ╎
╎  └──────────────────────────────┘                                    ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ Logger / EventSink           │   │ Config / IdGen               │ ╎
╎  │ [Effect logger/service]      │   │ [Effect services]            │ ╎
╎  │ platform/logging/            │   │ platform/config.ts;          │ ╎
╎  │ Dropping queue; batched      │   │ platform/sqlite.ts           │ ╎
╎  │ JSONL; final drain           │   │ Validated options            │ ╎
╎  └──────────────────────────────┘   │ (sqlite.library,             │ ╎
╎                                     │ sqlite.statementDeadlineMs   │ ╎
╎                                     │ and sqlite.statementInterrup │ ╎
╎                                     │ tGraceMs are all read);      │ ╎
╎                                     │ UUIDs; Clock                 │ ╎
╎                                     └──────────────────────────────┘ ╎
╎                                                                      ╎
╰╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╯

   ┌──────────────────────────────┐   ┌──────────────────────────────┐
   │ Central registry             │   │ Application files            │
   │ [Postgres process/store]     │   │ [SQLite 3 WAL]               │
   │ DATABASE_URL                 │   │ DATA_DIR; TENANTS_DIR        │
   │ Tenant/key and headless auth │   │ Legacy app files or tenant   │
   │ data                         │   │ locations                    │
   └──────────────────────────────┘   └──────────────────────────────┘

   ┌──────────────────────────────┐   ┌──────────────────────────────┐
   │ Reader child                 │   │ Object storage               │
   │ [Deno process]               │   │ [External system /           │
   │ platform/reader.ts           │   │ filesystem]                  │
   │ DuckDB event-log ingest and  │   │ S3_BASE_URL or               │
   │ named analysis               │   │ LOCAL_OBJECT_DIR             │
   └──────────────────────────────┘   │ Snapshot and manifest        │
                                      │ objects                      │
                                      └──────────────────────────────┘

   ┌──────────────────────────────┐
   │ Server JSONL                 │
   │ [Filesystem]                 │
   │ LOG_DIR/server/date/         │
   │ Durable structured records   │
   └──────────────────────────────┘
```

## Relationships

| #  | From                         | To                           | Relationship                                                                                                                              |
| -- | ---------------------------- | ---------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| 1  | HTTP API / auth              | AppRegistry / AppServices    | Resolve app and named handler                                                                                                             |
| 2  | HTTP API / auth              | TenantRegistry               | Resolve hashed credentials; admin tenant/key CRUD                                                                                         |
| 3  | HTTP API / auth              | Central registry             | Better Auth session/registration SQL                                                                                                      |
| 4  | HTTP API / auth              | TenantContext                | Provide authenticated tenant per request                                                                                                  |
| 5  | TenantContext                | Storage                      | Resolve tenant writer; never use caller selectors                                                                                         |
| 6  | AppRegistry / AppServices    | TenantStorageRouter          | Build per-app LocalFolderRouter                                                                                                           |
| 7  | TenantStorageRouter          | FFI SQLite binding           | Open and prepare validated tenant SQLite file; schema guard                                                                               |
| 8  | Storage                      | TenantStorageRouter          | Resolve/prepare tenant location before opening a cached writer                                                                            |
| 9  | HTTP API / auth              | Storage                      | Mutation Effect inside serialized transaction                                                                                             |
| 10 | HTTP API / auth              | DocumentReaderClient         | Document query in-process; compiled SQLite SQL                                                                                            |
| 11 | DocumentReaderClient         | AppRegistry / AppServices    | Resolve registered query, schema and file                                                                                                 |
| 12 | DocumentReaderClient         | FFI SQLite binding           | Read-only connection and transaction; point reads blocking, other selects allAsync under the deadline; prepared-statement cache           |
| 13 | DocumentReaderClient         | Statement classifier         | Pick the path of each select from its plan                                                                                                |
| 14 | HTTP API / auth              | ReaderClient / ReaderProcess | Ingest/analysis RPC; trace context                                                                                                        |
| 15 | Storage                      | FFI SQLite binding           | Writer connection, transactions and snapshot reads; writes and point reads blocking, unbounded reads allAsync/getAsync under the deadline |
| 16 | Storage                      | Statement classifier         | Pick the path of each handler read from its plan                                                                                          |
| 17 | Server lifecycle             | Backups / ObjectStore        | Scoped backup timer                                                                                                                       |
| 18 | Backups / ObjectStore        | Storage                      | Consistent VACUUM INTO snapshot                                                                                                           |
| 19 | Backups / ObjectStore        | FFI SQLite binding           | Read-only quick_check of the snapshot copy before publication                                                                             |
| 20 | Backups / ObjectStore        | Object storage               | Verified snapshot and manifest publication                                                                                                |
| 21 | TenantRegistry               | Central registry             | Parameterized registry SQL; lazy scoped pool                                                                                              |
| 22 | Server lifecycle             | ReaderClient / ReaderProcess | Ping; hourly ingest; reader-exit watch and finalizer                                                                                      |
| 23 | ReaderClient / ReaderProcess | Reader child                 | JSONL RPC; scoped child shutdown                                                                                                          |
| 24 | Storage                      | Logger / EventSink           | SQL/tx/queue events (each sql event carries its path); error lifecycle event for a stuck statement                                        |
| 25 | DocumentReaderClient         | Logger / EventSink           | SQL/tx events (with path); error lifecycle event for a stuck statement                                                                    |
| 26 | HTTP API / auth              | Logger / EventSink           | HTTP/handler spans and failures                                                                                                           |
| 27 | Server lifecycle             | Logger / EventSink           | Startup/shutdown and timer events                                                                                                         |
| 28 | Backups / ObjectStore        | Logger / EventSink           | Backup and object-transfer events                                                                                                         |
| 29 | Logger / EventSink           | Server JSONL                 | Batched append; rotation and scope drain                                                                                                  |
| 30 | Config / IdGen               | AppRegistry / AppServices    | App schemas, tokens and paths; generated ids                                                                                              |
| 31 | Config / IdGen               | TenantRegistry               | DATABASE_URL and scope-owned cache                                                                                                        |
| 32 | Config / IdGen               | TenantStorageRouter          | TENANTS_DIR and router selection                                                                                                          |
| 33 | Config / IdGen               | Logger / EventSink           | Level, queue, flush and rotation settings                                                                                                 |
| 34 | Config / IdGen               | FFI SQLite binding           | sqlite.library: the libsqlite3 path passed to every open                                                                                  |
| 35 | Config / IdGen               | Storage                      | sqlite.statementDeadlineMs and statementInterruptGraceMs: deadline and interrupt grace of unbounded reads                                 |
| 36 | Config / IdGen               | DocumentReaderClient         | sqlite.statementDeadlineMs and statementInterruptGraceMs: deadline and interrupt grace of unbounded selects                               |
| 37 | AppRegistry / AppServices    | FFI SQLite binding           | Load libsqlite3 once at startup; sqlite-library lifecycle event names path and version                                                    |
| 38 | FFI SQLite binding           | Application files            | libsqlite3 file I/O; WAL transactions and read-only snapshots                                                                             |

## Runtime notes

Config is acquired before logging, then IdGen, ObjectStore/AppRegistry, TenantRegistry, AppServices and
ReaderClient. Shutdown drains HTTP, stops timers, closes reader and DB handles, emits the final lifecycle
event, then drains logging. Every partial startup releases acquired resources. Auth derives TenantContext
before consuming app arguments; the tenant SQLite location is prepared behind a shared initialization barrier.
Tenant key revocation invalidates the local cache; other instances converge within its TTL. Tenant-mode
writers use a bounded scope-owned handle cache (default 64 handles, 60-second idle expiry); requests lease
handles, and shutdown drains leases before closing them. Retention and named analysis execute in the reader,
never as server-side DuckDB components. Item 26 removes compatibility wrappers and platform/compiler.ts; pure
compilation now lives in core/compile/ and introduces no new runtime component. HTTP status mapping lives in
core/errors.ts. Better Auth connections are bounded to each request/CLI operation, separate from the lazy
registry pool. FFI-04: Storage, DocumentReaderClient, TenantStorageRouter and the backup verifier open SQLite
only through the FFI binding, on Config.sqlite.library; node:sqlite is not in the server runtime graph
(tests/unit/boundaries_test.ts). Startup loads libsqlite3 once and logs a lifecycle event, phase
sqlite-library, with the resolved path and version. FFI-04b: every FFI open, read-only included, applies
node:sqlite-compatible connection defaults (double-quoted string literals off, foreign_keys on) before any
caller SQL, and loadLibrary refuses darwin/aarch64. FFI-05: the classifier (platform/sqlite_ffi/classify.ts)
picks one of two execution paths per document operation from the plan IR. Blocking: writes, primary-key
selects and the writer's pragmas, DDL, BEGIN/COMMIT/ROLLBACK and VACUUM INTO run on the main thread, never
interrupted. Nonblocking: every other select, count and countBy (and the writer's U+0000 probe of such a
select) runs allAsync/getAsync on Deno's blocking pool under Config.sqlite.statementDeadlineMs. At the
deadline the binding calls sqlite3_interrupt and the statement fails StatementDeadlineError, which
toHttpStatus maps to HTTP 504 (core/errors.ts). Writes never take the nonblocking path: an interrupt landing
after an async write committed would leave the commit standing while the caller saw a failure. The call-level
reader.callTimeoutMs guard stays; config validation keeps the statement deadline below it, so the deadline
fires first. A per-connection ConnectionGate (platform/sqlite.ts, shared by Storage and the document reader)
keeps one statement at a time on a connection: while an async read is in flight, every other statement (a
forked fiber's, COMMIT, the release's ROLLBACK) waits instead of failing the binding's busy refusal. A step
that has not returned Config.sqlite.statementInterruptGraceMs after its interrupt poisons the connection: an
error-level lifecycle event (phase "statement did not return after interrupt"), no further statements, and the
connection closes when the step returns. Every sql event carries path (blocking or nonblocking); since FFI-06
it is written to the JSONL line and to the nullable sql_events.path column (see the observability container
diagram).
