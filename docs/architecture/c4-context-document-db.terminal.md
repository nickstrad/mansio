# System Context

Redrawn for IR-6 (ADR 0013) on 2026-11-06 on `ir/06-cutover` from main `7cf4ba6`: the Deno platform is gone
and the server is the Go server in `server/`. Both formats are generated from
`diagrams/c4-context-document-db.py`; numbered relationships are identical. All boxes describe landed code.

<!-- c4-render: diagrams/c4-context-document-db.py -->

```text
╭╌ Mansio ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╮
╎                                                                      ╎
╎  ┌──────────────────────────────┐                                    ╎
╎  │ Mansio                       │                                    ╎
╎  │ [Software system]            │                                    ╎
╎  │ server/ (Go)                 │                                    ╎
╎  │ agentd, controld, edge; one  │                                    ╎
╎  │ SQLite database per (tenant, │                                    ╎
╎  │ app); IR over HTTP           │                                    ╎
╎  └──────────────────────────────┘                                    ╎
╎                                                                      ╎
╰╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╯

   ┌──────────────────────────────┐   ┌──────────────────────────────┐
   │ App developer                │   │ Application caller           │
   │ [Person]                     │   │ [Person]                     │
   │ app/; sdk/                   │   │ sdk/client.ts                │
   │ Declares schemas and Effect  │   │ Players and authenticated    │
   │ handlers; the handlers run   │   │ tenant users                 │
   │ in the SDK                   │   └──────────────────────────────┘
   └──────────────────────────────┘

   ┌──────────────────────────────┐   ┌──────────────────────────────┐
   │ Operator                     │   │ Object storage               │
   │ [Person]                     │   │ [External system]            │
   │ scripts/tenants.ts;          │   │ S3 (SeaweedFS in dev)        │
   │ server/cmd/dbtool            │   │ SQLite generations and       │
   │ Tenants, keys, placement,    │   │ manifests (durable mode)     │
   │ backups; reads the request   │   └──────────────────────────────┘
   │ log and metrics              │
   └──────────────────────────────┘

   ┌──────────────────────────────┐
   │ k6 load generator            │
   │ [External system]            │
   │ perf/*.js                    │
   │ IR load over /v1/ops; raw    │
   │ metric points                │
   └──────────────────────────────┘
```

## Relationships

| # | From               | To             | Relationship                                                                         |
| - | ------------------ | -------------- | ------------------------------------------------------------------------------------ |
| 1 | App developer      | Mansio         | Registered schema JSON; handlers stay in the app (TypeScript SDK)                    |
| 2 | Application caller | Mansio         | IR over HTTP (/v1/resolve, /v1/ops); tickets, API keys or sessions; X-Trace-Id       |
| 3 | Operator           | Mansio         | Admin HTTP and CLIs; tenants, keys, moves, backups; request log (JSONL) and /metrics |
| 4 | Mansio             | Object storage | S3 HTTP; write-back of SQLite generations, restore                                   |
| 5 | k6 load generator  | Mansio         | IR scenarios over /v1/ops and trace headers                                          |

## Runtime notes

Since IR-6 the server is the Go server in server/; the Deno platform was deleted (ADR 0013). App handlers run
in the client SDK and reach the server as IR requests (ir/spec/v1.md); no application SQL crosses the client
boundary. Tenant identity comes from tickets, API keys or sessions (ADR 0012); Postgres is inside the system,
shown at Level 2. No external identity provider is wired. The request log and the metrics are for the
deployment's own log collector and Prometheus, which are not part of mansio (ADR 0014).
