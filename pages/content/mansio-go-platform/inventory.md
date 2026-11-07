# Conversion inventory: `mansio-go-platform`

Bundle: [public/architectures/mansio-go-platform.json](../../public/architectures/mansio-go-platform.json).
Built 2026-11-07 for Nick's C4 dataset directive. Revision: `main-6330c9f-go-platform`, describing
`6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc`. Awaiting Opus final pass and coordinator landing.

## Sources

Every URL and path below was checked with `git cat-file -e <pin>:<path>`. All sources use the baseline above.
Source `notes` is explicit because the video framework decoder requires it, although older Pages bundles omit
it.

| Source id       | Pinned file                                                                                                                                                                                                                | Evidence                                           |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| `src-adr`       | [docs/adr/0013-go-server-replaces-deno-platform.md](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/docs/adr/0013-go-server-replaces-deno-platform.md)                                   | Go-only backend and offline analysis               |
| `src-recording` | [sdk/recording.ts](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/sdk/recording.ts)                                                                                                     | Caller-side handler recording                      |
| `src-transport` | [sdk/transport.ts](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/sdk/transport.ts)                                                                                                     | Cached locator routes and retries                  |
| `src-agent`     | [server/cmd/agentd/main.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/cmd/agentd/main.go)                                                                                   | Agent daemon and server wiring                     |
| `src-edge`      | [server/cmd/edge/main.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/cmd/edge/main.go)                                                                                       | Separate TLS edge process                          |
| `src-controld`  | [server/cmd/controld/main.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/cmd/controld/main.go)                                                                               | Control daemon and operator commands               |
| `src-locator`   | [server/internal/control/locator/doc.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/control/locator/doc.go)                                                         | Routing and signed tickets                         |
| `src-control`   | [server/internal/control/controller.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/control/controller.go)                                                           | Database lifecycle and attest-dead recovery        |
| `src-placement` | [server/internal/placement/pg/doc.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/placement/pg/doc.go)                                                               | Postgres epoch authority                           |
| `src-move`      | [server/internal/owner/move.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/owner/move.go)                                                                           | Four-step move protocol                            |
| `src-owner`     | [server/internal/owner/state.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/owner/state.go)                                                                         | Admission, fencing and storage epochs              |
| `src-actor`     | [server/internal/actor/actor.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/actor/actor.go)                                                                         | Mailbox, single writer and group commit            |
| `src-reqid`     | [server/internal/actor/reqid.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/actor/reqid.go)                                                                         | Request-id savepoint claims                        |
| `src-ops`       | [server/internal/front/ops.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/front/ops.go)                                                                             | IR HTTP validation and actor dispatch              |
| `src-exec`      | [server/internal/docexec/doc.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/docexec/doc.go)                                                                         | IR execution on the actor connection               |
| `src-compile`   | [server/internal/doccompile/doc.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/doccompile/doc.go)                                                                   | Document SQL compiler                              |
| `src-writeback` | [server/internal/vactor/writeback.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/vactor/writeback.go)                                                               | Snapshot write-back and epoch stamps               |
| `src-durable`   | [server/internal/durable/durable.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/durable/durable.go)                                                                 | Immutable generations and conditional manifest     |
| `src-paused`    | [server/knowledge/writeback/2026-11-06-backup-paused-open-issues.md](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/knowledge/writeback/2026-11-06-backup-paused-open-issues.md) | Built S3 work paused on 2026-11-06                 |
| `src-events`    | [server/internal/eventlog/log.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/internal/eventlog/log.go)                                                                       | Nonblocking structured JSONL sink                  |
| `src-ingest`    | [eventlog/ingest.ts](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/eventlog/ingest.ts)                                                                                                 | Offline DuckDB ingest                              |
| `src-log-adr`   | [docs/adr/0004-eventlog-duckdb-in-reader-only.md](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/docs/adr/0004-eventlog-duckdb-in-reader-only.md)                                       | JSONL authority and rebuildable analysis           |
| `src-topics`    | [docs/cache-dividend-topics.md](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/docs/cache-dividend-topics.md)                                                                           | Current platform survey and implementation caveats |
| `src-observer`  | [server/cmd/agentd/events.go](https://github.com/nickstrad/mansio/blob/6330c9f4fb4fc2be5ee28caa6bb27209dc85ddcc/server/cmd/agentd/events.go)                                                                               | Agentd IR event observer wiring                    |

## Conversion notes and deliberate exclusions

- This is an additive code-derived snapshot. The four earlier bundles and the site default remain unchanged.
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
- Attest-dead and restore orchestration belong to controld. Target agentd performs claim/restore and file
  work.
- Canonical JSONL means the persisted emitted record, not guaranteed lossless logging. The bounded sink can
  drop events. DuckDB can be rebuilt only from retained input; compressed input needs decompression.
- No design-stage lanes, private worker processes, proposed leases or planes components were imported as
  built. Historical alternatives remain in [lanes](../mansio-lanes-proposal/inventory.md),
  [planes](../mansio-planes-proposal/inventory.md) and [Fable planes](../mansio-planes-fable/inventory.md).
- STE text is an explicitly unreviewed draft, not a controlled-English compliance claim.
- External tags denote supporting elements outside a view scope, not third-party ownership.
- The component view separates actor execution from durability/event adapters for readability. The compiler's
  SQLite relationship means execution through the actor-owned Tx port, not a separate compiler connection.

## Element mapping

| Element id       | Kind / parent              | Evidence source ids                          |
| ---------------- | -------------------------- | -------------------------------------------- |
| `developer`      | person / root              | `src-recording`, `src-transport`             |
| `operator`       | person / root              | `src-controld`, `src-adr`                    |
| `platform`       | softwareSystem / root      | `src-adr`, `src-control`, `src-topics`       |
| `application`    | softwareSystem / root      | `src-recording`                              |
| `caller-process` | container / application    | `src-recording`                              |
| `sdk`            | component / caller-process | `src-recording`, `src-transport`             |
| `s3`             | softwareSystem / root      | `src-durable`, `src-paused`                  |
| `controld`       | container / platform       | `src-controld`, `src-locator`, `src-control` |
| `edge`           | container / platform       | `src-edge`                                   |
| `agentd`         | container / platform       | `src-agent`, `src-owner`                     |
| `files`          | container / platform       | `src-actor`, `src-reqid`, `src-control`      |
| `metadata`       | container / platform       | `src-placement`, `src-agent`                 |
| `jsonl`          | container / platform       | `src-events`, `src-adr`, `src-log-adr`       |
| `analysis`       | container / platform       | `src-adr`, `src-ingest`                      |
| `duckdb`         | container / platform       | `src-ingest`, `src-log-adr`                  |
| `admission`      | component / agentd         | `src-agent`, `src-ops`, `src-owner`          |
| `ir`             | component / agentd         | `src-ops`, `src-exec`                        |
| `compiler`       | component / agentd         | `src-compile`, `src-exec`                    |
| `mailbox`        | component / agentd         | `src-actor`                                  |
| `actor`          | component / agentd         | `src-actor`, `src-exec`                      |
| `writer`         | component / agentd         | `src-actor`                                  |
| `requests`       | component / agentd         | `src-reqid`, `src-actor`                     |
| `storage`        | component / agentd         | `src-writeback`, `src-durable`, `src-paused` |
| `events`         | component / agentd         | `src-events`                                 |
| `locator`        | component / controld       | `src-locator`                                |
| `authority`      | component / controld       | `src-placement`, `src-move`                  |
| `signer`         | component / controld       | `src-locator`, `src-transport`               |
| `lifecycle`      | component / controld       | `src-control`, `src-move`                    |
| `source-agent`   | container / platform       | `src-move`, `src-owner`                      |
| `target-agent`   | container / platform       | `src-move`, `src-owner`, `src-durable`       |

## Relationship mapping

| Relationship id       | Endpoints                 | Evidence source ids                                  |
| --------------------- | ------------------------- | ---------------------------------------------------- |
| `ctx-call`            | developer → application   | `src-recording`                                      |
| `ctx-use`             | application → platform    | `src-recording`, `src-ops`                           |
| `ctx-operate`         | operator → platform       | `src-controld`, `src-adr`                            |
| `ctx-durable`         | platform → s3             | `src-durable`, `src-paused`                          |
| `caller-sdk`          | developer → sdk           | `src-recording`                                      |
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
| `agent-log`           | agentd → jsonl            | `src-events`, `src-adr`                              |
| `log-ingest`          | jsonl → analysis          | `src-ingest`, `src-adr`                              |
| `analysis-db`         | analysis → duckdb         | `src-ingest`                                         |
| `operator-analysis`   | operator → analysis       | `src-adr`                                            |
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
| `events-jsonl`        | events → jsonl            | `src-events`                                         |
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

## Views

| Id                  | Kind      | Subject                                                   |
| ------------------- | --------- | --------------------------------------------------------- |
| `context`           | context   | mansio Go platform: context                               |
| `containers`        | container | Request path: caller → control plane once → data plane    |
| `caller-components` | component | Caller process: in-process SDK and handler retries        |
| `components`        | component | agentd: admission, IR and per-database execution          |
| `storage-events`    | component | agentd: paused durability and canonical events            |
| `control-plane`     | component | controld: locator, signer and epoch authority             |
| `observability`     | container | Canonical JSONL → offline DuckDB analysis                 |
| `query-flow`        | dynamic   | Query: resolve once, then cached-ticket data path         |
| `database-move`     | dynamic   | Database move: fence before epoch flip (S3 built, paused) |

Compact node descriptions keep cards readable. Element documents retain the fuller evidence notes.
