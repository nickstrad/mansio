# Container — request path

Redrawn for IR-6 (ADR 0013) on 2026-11-06 on `ir/06-cutover` from main `7cf4ba6`: the Deno platform is gone
and the server is the Go server in `server/`. Both formats are generated from
`diagrams/c4-container-platform.py`; numbered relationships are identical. All boxes describe landed code.

<!-- c4-render: diagrams/c4-container-platform.py -->

```text
╭╌ Mansio cluster ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╮
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ Edge                         │   │ Agent                        │ ╎
╎  │ [Go process]                 │   │ [Go process]                 │ ╎
╎  │ server/cmd/edge              │   │ server/cmd/agentd            │ ╎
╎  │ Forwards /v1/ops to the      │   │ IR front (/v1/ops); one      │ ╎
╎  │ ticket's VM; registry from   │   │ actor per database; static,  │ ╎
╎  │ controld                     │   │ tenancy or durable mode      │ ╎
╎  └──────────────────────────────┘   └──────────────────────────────┘ ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ Databases                    │   │ Control plane                │ ╎
╎  │ [SQLite 3 files]             │   │ [Go process]                 │ ╎
╎  │ agentd db_dir                │   │ server/cmd/controld          │ ╎
╎  │ One file per (tenant, app):  │   │ Resolve and tickets; create, │ ╎
╎  │ tenant:app.db                │   │ move, attest-dead, restore;  │ ╎
╎  └──────────────────────────────┘   │ tenancy admin and auth       │ ╎
╎                                     └──────────────────────────────┘ ╎
╎                                                                      ╎
╎  ┌──────────────────────────────┐   ┌──────────────────────────────┐ ╎
╎  │ Control store                │   │ Operator CLIs                │ ╎
╎  │ [Postgres 17]                │   │ [Go and Deno processes]      │ ╎
╎  │ schema control               │   │ server/cmd/dbtool;           │ ╎
╎  │ Placement, issuer keys,      │   │ scripts/tenants.ts           │ ╎
╎  │ tenants, API keys, users,    │   │ Restore, import, verify;     │ ╎
╎  │ sessions                     │   │ tenant and key admin         │ ╎
╎  └──────────────────────────────┘   └──────────────────────────────┘ ╎
╎                                                                      ╎
╰╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╯

   ┌──────────────────────────────┐   ┌──────────────────────────────┐
   │ Client SDK                   │   │ Object storage               │
   │ [Library in caller process]  │   │ [External system]            │
   │ sdk/client.ts;               │   │ S3 (SeaweedFS in dev)        │
   │ sdk/transport.ts             │   │ SQLite generations and       │
   │ Runs handlers; sends IR;     │   │ manifests                    │
   │ resolves and caches tickets  │   └──────────────────────────────┘
   └──────────────────────────────┘
```

## Relationships

| #  | From          | To             | Relationship                                                        |
| -- | ------------- | -------------- | ------------------------------------------------------------------- |
| 1  | Client SDK    | Control plane  | HTTP POST /v1/resolve; API key or session; ticket and endpoint back |
| 2  | Client SDK    | Edge           | HTTP POST /v1/ops (IR); ticket; X-Trace-Id                          |
| 3  | Edge          | Agent          | HTTP /v1/ops, routed by the ticket's VM                             |
| 4  | Edge          | Control plane  | HTTP VM registry and ticket keys                                    |
| 5  | Agent         | Databases      | modernc SQLite; group-committed writes and reads                    |
| 6  | Agent         | Object storage | S3 HTTP; write-back of generations, restore on claim                |
| 7  | Agent         | Control plane  | HTTP ticket keys and registered schemas (keys_url, schemas_url)     |
| 8  | Control plane | Agent          | HTTP lifecycle: claim, release, move                                |
| 9  | Agent         | Control store  | Postgres protocol; placement rows (placement_pg)                    |
| 10 | Control plane | Control store  | Postgres protocol; placement, tenancy, issuer                       |
| 11 | Operator CLIs | Object storage | S3 HTTP; dbtool restore, import, verify                             |
| 12 | Operator CLIs | Agent          | Admin HTTP; /admin/tenants, /admin/backup                           |

## Runtime notes

This is the multi-node shape (IR-5a, e2e scale_out). A single agentd also runs alone: static mode (a keys
file, minted tickets, files it finds in db_dir), with a tenancy section (app tokens, API keys or sessions on
/v1/ops, its own control schema in Postgres) or durable (S3 generations, placement_dir or placement_pg); then
the SDK sends /v1/ops straight to it and there is no edge or controld. Writes are acknowledged before
write-back, so a kill -9 can lose the window (intermediate-representation-protocol.md §9). scripts/tenants.ts
talks to whichever process mounts /admin/tenants (agentd or controld).
