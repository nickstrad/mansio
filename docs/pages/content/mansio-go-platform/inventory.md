# Conversion inventory: `mansio-go-platform`

Bundle: [public/architectures/mansio-go-platform.json](../../public/architectures/mansio-go-platform.json).
Built 2026-11-07 for Nick's C4 dataset directive at `6330c9f`; refreshed 2026-11-11 to revision
`main-bffb5e9-go-platform`, describing `bffb5e9face4a4e83839f6e8fbd0819a5a503dc6`. Since 2026-11-11 this is
the **only** dataset on the site: main is Go for the control and data planes plus the TypeScript SDK, and the
Deno-era `mansio-current` and the lanes/planes/Fable-planes proposals were removed (they remain in git
history). Refreshed 2026-11-12 to revision `main-636cc30-go-platform`, describing
`636cc30070cec752bb5773449f9f04d3a03c8e8a`: the offline event-log tool is removed (ADR 0014), the operator
tools are `mansioctl` and `dbtool`, and the notes moved to undated topic files. Every source is pinned to that
commit.

## Sources

Every URL and path below was checked with `git cat-file -e <pin>:<path>`. Sources use the latest pin named
above. Source `notes` is explicit because the video framework decoder requires it, although older Pages
bundles omit it.

| Source id           | Pinned file                                                                                                                                                                                              | Evidence                                                  |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| `src-adr`           | [docs/adr/0013-go-server-replaces-deno-platform.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/docs/adr/0013-go-server-replaces-deno-platform.md)                 | Go-only backend (ADR 0013)                                |
| `src-recording`     | [sdk/recording.ts](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/sdk/recording.ts)                                                                                   | Caller-side handler recording                             |
| `src-transport`     | [sdk/transport.ts](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/sdk/transport.ts)                                                                                   | Cached locator routes and retries                         |
| `src-agent`         | [server/cmd/agentd/main.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/cmd/agentd/main.go)                                                                 | Agent daemon and server wiring                            |
| `src-edge`          | [server/cmd/edge/main.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/cmd/edge/main.go)                                                                     | Separate TLS edge process                                 |
| `src-controld`      | [server/cmd/controld/main.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/cmd/controld/main.go)                                                             | Control daemon and operator commands                      |
| `src-locator`       | [server/internal/control/locator/doc.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/control/locator/doc.go)                                       | Routing and signed tickets                                |
| `src-control`       | [server/internal/control/controller.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/control/controller.go)                                         | Database lifecycle and attest-dead recovery               |
| `src-placement`     | [server/internal/placement/pg/doc.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/placement/pg/doc.go)                                             | Postgres epoch authority                                  |
| `src-move`          | [server/internal/owner/move.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/owner/move.go)                                                         | Four-step move protocol                                   |
| `src-owner`         | [server/internal/owner/state.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/owner/state.go)                                                       | Admission, fencing and storage epochs                     |
| `src-actor`         | [server/internal/actor/actor.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/actor/actor.go)                                                       | Mailbox, single writer and group commit                   |
| `src-reqid`         | [server/internal/actor/reqid.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/actor/reqid.go)                                                       | Request-id savepoint claims                               |
| `src-ops`           | [server/internal/front/ops.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/front/ops.go)                                                           | IR HTTP validation and actor dispatch                     |
| `src-exec`          | [server/internal/docexec/doc.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/docexec/doc.go)                                                       | IR execution on the actor connection                      |
| `src-compile`       | [server/internal/doccompile/doc.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/doccompile/doc.go)                                                 | Document SQL compiler                                     |
| `src-writeback`     | [server/internal/vactor/writeback.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/vactor/writeback.go)                                             | Snapshot write-back and epoch stamps                      |
| `src-durable`       | [server/internal/durable/durable.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/durable/durable.go)                                               | Immutable generations and conditional manifest            |
| `src-paused`        | [server/docs/notes/writeback/backup-open-issues.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/docs/notes/writeback/backup-open-issues.md)                 | Built S3 work paused on 2026-11-06                        |
| `src-events`        | [server/internal/eventlog/log.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/eventlog/log.go)                                                     | Nonblocking request log writer                            |
| `src-log-decision`  | [docs/adr/0014-request-log-without-an-offline-tool.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/docs/adr/0014-request-log-without-an-offline-tool.md)           | Request log shipped, no offline tool (ADR 0014)           |
| `src-request-log`   | [docs/observability.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/docs/observability.md)                                                                         | Request log reference: records, limits, shipping, metrics |
| `src-observer`      | [server/cmd/agentd/events.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/cmd/agentd/events.go)                                                             | Agentd IR event observer wiring                           |
| `src-agent-durable` | [server/cmd/agentd/durable.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/cmd/agentd/durable.go)                                                           | Durable mode: S3 host and Postgres/file authority         |
| `src-control-ports` | [server/internal/control/ports.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/control/ports.go)                                                   | Drop deletes a database's S3 objects                      |
| `src-cache`         | [sdk/cache.ts](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/sdk/cache.ts)                                                                                           | Version-checked client read cache                         |
| `src-read-path`     | [docs/notes/ir/read-path.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/docs/notes/ir/read-path.md)                                                               | Read path after survey changes 1-4                        |
| `src-retry`         | [docs/retry-contract.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/docs/retry-contract.md)                                                                       | One retry contract for both clients                       |
| `src-admission`     | [server/internal/front/admission.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/front/admission.go)                                               | Admission caps and would-expire shedding                  |
| `src-edge-proxy`    | [server/internal/edge/proxy.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/edge/proxy.go)                                                         | Edge refusals before the body is read                     |
| `src-locator-gates` | [server/internal/control/locator/locator.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/control/locator/locator.go)                               | Locator resolve, load and create gates                    |
| `src-seq`           | [server/internal/docexec/seq.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/docexec/seq.go)                                                       | Commit sequence and its floor                             |
| `src-version`       | [server/internal/docexec/version.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/docexec/version.go)                                               | Version floor per incarnation                             |
| `src-witness`       | [server/internal/docexec/witness.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/docexec/witness.go)                                               | Conflict answers carry current state                      |
| `src-version-floor` | [server/docs/notes/writeback/version-and-sequence-floor.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/docs/notes/writeback/version-and-sequence-floor.md) | Version and sequence floor after rollback                 |
| `src-retention`     | [server/internal/eventlog/retention.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/eventlog/retention.go)                                         | Event-log retention in the writer                         |
| `src-queues`        | [server/internal/metrics/queue.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/internal/metrics/queue.go)                                                   | Per-queue metric families                                 |
| `src-capacity`      | [server/docs/capacity.md](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/docs/capacity.md)                                                                     | Capacity table, alerts and levers                         |
| `src-mansioctl`     | [server/cmd/mansioctl/main.go](https://github.com/nickstrad/mansio/blob/636cc30070cec752bb5773449f9f04d3a03c8e8a/server/cmd/mansioctl/main.go)                                                           | The platform admin CLI                                    |

## Conversion notes and deliberate exclusions

- This is a code-derived snapshot of `main` and the site default. Keep it in sync with `main` (see
  `docs/pages/README.md`, "Keeping main in sync").
- Refresh to `bffb5e9` (2026-11-11), from the 71 commits after `6330c9f`: backpressure B1–B11 (edge refusals
  before the body and `max_in_flight`; admission's per-database/tenant/VM caps shedding what would expire; the
  locator's narrow load/create gates under the Postgres pool; writer-side event-log retention; per-queue
  `mansio_queue_*` metrics and the shipped alert rules; one retry contract), the read path (survey changes
  1–4: batched reads at one snapshot, guarded `find`, conflict answers with `seq` and current state, and the
  version-checked client read cache, drawn as the new `read-cache` component and `sdk-cache` relationship),
  and the version and sequence floors (spec revisions 1.2 and 1.3; the "version reuse after rollback is open"
  claim is removed). The SDK's "find is unguarded in v1" claim is replaced: finds are guarded by a table
  assert unless `.unguarded()`. Metrics are described in documents only: no Prometheus server is part of the
  platform, only `server/deploy/prometheus/backpressure.rules.yml`. `make platform-up` (a local dev cluster)
  adds no runtime element and is not drawn.
- `server/docs/architecture-c4.md` and `server/README.md` describe sqlbench, not the complete current
  platform. Their actor story was checked against the current actor code; the platform boundaries come from
  command wiring.
- `agentd` is the Go server and per-VM daemon. `edge` is a separate TLS proxy. There is no extra server
  process behind agentd. The SDK is a component inside the caller process, never a network service.
- The server validates and executes IR; the SDK re-runs application handlers after eligible conflicts.
  “Resolve once” means once per cached route, with TTL refresh and stale-owner invalidation.
- S3 and write-back use `status: existing`, tag `implemented-paused`, explicit names, descriptions,
  relationship labels and diagram notes. Existing means built, not live. No unsupported `paused` status is
  invented. Work was paused on 2026-11-06 pending a LiteFS decision. A crash can lose acknowledged writes;
  version reuse after rollback remains open. No zero-RPO guarantee is implied. WAL shipping and LiteFS are not
  implemented.
- Source and target in the move view are explicitly two runtime roles of the same agentd executable. This is a
  logical flow, not a deployment topology. No deployment view is asserted.
- agentd has two modes: static (local files at fixed epochs, no moves) and durable (S3 host plus placement
  authority). Lifecycle RPC, the agent's placement connection, moves and restore exist in durable mode only,
  so they depend on the built-but-paused S3 path. Relationship labels say so.
- Attest-dead and restore orchestration belong to controld. Target agentd performs claim/restore and file
  work.
- The request log (`jsonl`, named "Request log (JSONL)") is the persisted emitted record, not guaranteed
  lossless logging: the bounded sink can drop records, and the writer deletes its oldest files at its limits.
- Observability without the offline tool (2026-11-12, ADR 0014): the offline log CLI (`analysis`) and the
  derived DuckDB copy (`duckdb`) are deleted with their relationships (`log-ingest`, `analysis-db`,
  `operator-analysis`), document (`doc-element-duckdb`) and sources (`src-ingest`, `src-log-adr`). The
  `observability` view now draws agentd writing the request log and the operator reading or shipping it
  (`operator-log`, new); `controld` and `operator-control` left that view (they are drawn in the move view).
  The metrics endpoints are described in its document only: no log store, log collector or Prometheus server
  is part of the platform, so none is drawn. New sources: `src-log-decision` (ADR 0014) and `src-request-log`
  (`docs/observability.md`).
- No design-stage lanes, private worker processes, proposed leases or planes components were imported as
  built. The historical proposal datasets were removed from the site on 2026-11-11 (see git history of
  `docs/pages/content/`).
- STE text is an explicitly unreviewed draft, not a controlled-English compliance claim.
- External tags denote supporting elements outside a view scope, not third-party ownership. Tags are global
  per element, so only the elements the c4-sdk validator requires carry `external` (supporting containers or
  components in another view's scope), plus the caller application. In-scope components and the platform do
  not.
- The component view separates actor execution from durability/event adapters for readability. The compiler's
  SQLite relationship means execution through the actor-owned Tx port, not a separate compiler connection.

## Element mapping

| Element id       | Kind / parent              | Evidence source ids                                                  |
| ---------------- | -------------------------- | -------------------------------------------------------------------- |
| `developer`      | person / root              | `src-recording`, `src-transport`                                     |
| `operator`       | person / root              | `src-controld`, `src-adr`, `src-request-log`, `src-mansioctl`        |
| `platform`       | softwareSystem / root      | `src-adr`, `src-control`                                             |
| `application`    | softwareSystem / root      | `src-recording`                                                      |
| `caller-process` | container / application    | `src-recording`                                                      |
| `sdk`            | component / caller-process | `src-recording`, `src-transport`                                     |
| `read-cache`     | component / caller-process | `src-cache`, `src-read-path`                                         |
| `s3`             | softwareSystem / root      | `src-durable`, `src-paused`                                          |
| `controld`       | container / platform       | `src-controld`, `src-locator`, `src-control`                         |
| `edge`           | container / platform       | `src-edge`                                                           |
| `agentd`         | container / platform       | `src-agent`, `src-owner`, `src-agent-durable`                        |
| `files`          | container / platform       | `src-actor`, `src-reqid`, `src-control`                              |
| `metadata`       | container / platform       | `src-placement`, `src-agent`                                         |
| `jsonl`          | container / platform       | `src-events`, `src-log-decision`, `src-request-log`, `src-retention` |
| `admission`      | component / agentd         | `src-agent`, `src-ops`, `src-owner`                                  |
| `ir`             | component / agentd         | `src-ops`, `src-exec`                                                |
| `compiler`       | component / agentd         | `src-compile`, `src-exec`                                            |
| `mailbox`        | component / agentd         | `src-actor`                                                          |
| `actor`          | component / agentd         | `src-actor`, `src-exec`                                              |
| `writer`         | component / agentd         | `src-actor`                                                          |
| `requests`       | component / agentd         | `src-reqid`, `src-actor`                                             |
| `storage`        | component / agentd         | `src-writeback`, `src-durable`, `src-paused`                         |
| `events`         | component / agentd         | `src-events`                                                         |
| `locator`        | component / controld       | `src-locator`                                                        |
| `authority`      | component / controld       | `src-placement`, `src-move`                                          |
| `signer`         | component / controld       | `src-locator`, `src-transport`                                       |
| `lifecycle`      | component / controld       | `src-control`, `src-move`, `src-controld`                            |
| `source-agent`   | container / platform       | `src-move`, `src-owner`                                              |
| `target-agent`   | container / platform       | `src-move`, `src-owner`, `src-durable`                               |

## Relationship mapping

| Relationship id       | Endpoints                 | Evidence source ids                                  |
| --------------------- | ------------------------- | ---------------------------------------------------- |
| `ctx-call`            | developer → application   | `src-recording`                                      |
| `ctx-use`             | application → platform    | `src-recording`, `src-ops`                           |
| `ctx-operate`         | operator → platform       | `src-controld`, `src-adr`, `src-request-log`         |
| `ctx-durable`         | platform → s3             | `src-durable`, `src-paused`                          |
| `caller-sdk`          | developer → sdk           | `src-recording`                                      |
| `sdk-cache`           | sdk → read-cache          | `src-cache`, `src-read-path`                         |
| `caller-resolve`      | caller-process → controld | `src-transport`, `src-locator`                       |
| `caller-edge`         | caller-process → edge     | `src-transport`, `src-edge`, `src-ops`               |
| `sdk-resolve`         | sdk → controld            | `src-transport`, `src-locator`                       |
| `sdk-edge`            | sdk → edge                | `src-transport`, `src-edge`, `src-ops`               |
| `edge-agent`          | edge → agentd             | `src-edge`, `src-ops`                                |
| `agent-files`         | agentd → files            | `src-agent`, `src-actor`                             |
| `agent-s3`            | agentd → s3               | `src-agent`, `src-durable`, `src-paused`             |
| `control-agent`       | controld → agentd         | `src-control`, `src-move`, `src-agent`               |
| `control-meta`        | controld → metadata       | `src-placement`, `src-control`                       |
| `operator-control`    | operator → controld       | `src-controld`, `src-control`                        |
| `agent-log`           | agentd → jsonl            | `src-events`, `src-observer`, `src-request-log`      |
| `operator-log`        | operator → jsonl          | `src-request-log`, `src-log-decision`                |
| `edge-admit`          | edge → admission          | `src-edge`, `src-ops`                                |
| `admit-ir`            | admission → ir            | `src-ops`, `src-owner`                               |
| `ir-mail`             | ir → mailbox              | `src-ops`, `src-exec`, `src-actor`                   |
| `mail-actor`          | mailbox → actor           | `src-actor`                                          |
| `actor-ir`            | actor → ir                | `src-exec`, `src-actor`                              |
| `ir-compile`          | ir → compiler             | `src-exec`, `src-compile`                            |
| `compile-files`       | compiler → files          | `src-exec`, `src-compile`                            |
| `actor-write`         | actor → writer            | `src-actor`                                          |
| `writer-request`      | writer → requests         | `src-actor`, `src-reqid`                             |
| `request-files`       | requests → files          | `src-reqid`                                          |
| `writer-files`        | writer → files            | `src-actor`                                          |
| `storage-files`       | storage → files           | `src-writeback`, `src-durable`, `src-paused`         |
| `storage-s3`          | storage → s3              | `src-durable`, `src-paused`                          |
| `ir-events`           | ir → events               | `src-ops`, `src-events`, `src-observer`              |
| `events-jsonl`        | events → jsonl            | `src-events`, `src-retention`                        |
| `control-locator`     | sdk → locator             | `src-transport`, `src-locator`                       |
| `locator-authority`   | locator → authority       | `src-locator`, `src-placement`                       |
| `authority-meta`      | authority → metadata      | `src-placement`                                      |
| `locator-signer`      | locator → signer          | `src-locator`                                        |
| `op-lifecycle`        | operator → lifecycle      | `src-controld`, `src-control`                        |
| `lifecycle-authority` | lifecycle → authority     | `src-move`, `src-control`                            |
| `lifecycle-agent`     | lifecycle → agentd        | `src-control`, `src-move`                            |
| `move-begin`          | controld → metadata       | `src-move`, `src-placement`                          |
| `move-fence`          | controld → source-agent   | `src-move`, `src-owner`                              |
| `move-publish`        | source-agent → s3         | `src-move`, `src-owner`, `src-durable`, `src-paused` |
| `move-flip`           | controld → metadata       | `src-move`, `src-placement`                          |
| `move-claim`          | controld → target-agent   | `src-move`, `src-owner`                              |
| `move-read`           | target-agent → metadata   | `src-owner`, `src-placement`                         |
| `move-stamp`          | target-agent → s3         | `src-owner`, `src-durable`, `src-paused`             |
| `agent-keys`          | agentd → controld         | `src-agent`                                          |
| `edge-registry`       | edge → controld           | `src-edge`                                           |
| `agent-authority`     | agentd → metadata         | `src-agent-durable`, `src-owner`                     |
| `control-s3`          | controld → s3             | `src-control-ports`, `src-paused`                    |

## Views

| Id                  | Kind      | Subject                                                   |
| ------------------- | --------- | --------------------------------------------------------- |
| `context`           | context   | mansio Go platform: context                               |
| `containers`        | container | Request path: caller → control plane once → data plane    |
| `caller-components` | component | Caller process: in-process SDK and handler retries        |
| `components`        | component | agentd: admission, IR and per-database execution          |
| `storage-events`    | component | agentd: paused durability and canonical events            |
| `control-plane`     | component | controld: locator, signer and epoch authority             |
| `observability`     | container | Request log (JSONL) and metrics                           |
| `query-flow`        | dynamic   | Query: resolve once, then cached-ticket data path         |
| `database-move`     | dynamic   | Database move: fence before epoch flip (S3 built, paused) |

Compact node descriptions keep cards readable. Element documents retain the fuller evidence notes.
