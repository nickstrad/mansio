# System Context

Redrawn for IR-6 (ADR 0013) on 2026-11-06 on `ir/06-cutover` from main `7cf4ba6`: the Deno platform is gone
and the server is the Go server in `server/`. Both formats are generated from
`diagrams/c4-context-document-db.py`; numbered relationships are identical. All boxes describe landed code.

```mermaid
flowchart TB
    subgraph platform["Mansio"]
        db["Mansio<br/>[Software system]<br/>server/ (Go)<br/>agentd, controld, edge; one SQLite database per (tenant, app); IR over HTTP"]
    end
        developer["App developer<br/>[Person]<br/>app/; sdk/<br/>Declares schemas and Effect handlers; the handlers run in the SDK"]
        caller["Application caller<br/>[Person]<br/>sdk/client.ts<br/>Players and authenticated tenant users"]
        operator["Operator<br/>[Person]<br/>server/cmd/mansioctl; server/cmd/dbtool<br/>Tenants, keys, placement, backups and log analysis"]
        objects["Object storage<br/>[External system]<br/>S3 (SeaweedFS in dev)<br/>SQLite generations and manifests (durable mode)"]
        analytics["Analytics consumer<br/>[External system]<br/>scripts/logs.ts; scripts/duckdb/<br/>Offline DuckDB event log: named catalog or SQL"]
        k6["k6 load generator<br/>[External system]<br/>perf/*.js<br/>IR load over /v1/ops; raw metric points"]
    developer -->|"(1) Registered schema JSON; handlers stay in the app (TypeScript SDK)"| db
    caller -->|"(2) IR over HTTP (/v1/resolve, /v1/ops); tickets, API keys or sessions; X-Trace-Id"| db
    operator -->|"(3) Admin HTTP and CLIs; tenants, keys, moves, backups"| db
    db -->|"(4) S3 HTTP; write-back of SQLite generations, restore"| objects
    db -->|"(5) JSONL event log, ingested offline into DuckDB"| analytics
    k6 -->|"(6) IR scenarios over /v1/ops and trace headers"| db
    classDef planned stroke-dasharray: 5 3
    classDef person fill:#fff,stroke-width:2px
    classDef external fill:#f4f4f4
    class developer person
    class caller person
    class operator person
    class objects external
    class analytics external
    class k6 external
```

## Relationships

| # | From               | To                 | Relationship                                                                   |
| - | ------------------ | ------------------ | ------------------------------------------------------------------------------ |
| 1 | App developer      | Mansio             | Registered schema JSON; handlers stay in the app (TypeScript SDK)              |
| 2 | Application caller | Mansio             | IR over HTTP (/v1/resolve, /v1/ops); tickets, API keys or sessions; X-Trace-Id |
| 3 | Operator           | Mansio             | Admin HTTP and CLIs; tenants, keys, moves, backups                             |
| 4 | Mansio             | Object storage     | S3 HTTP; write-back of SQLite generations, restore                             |
| 5 | Mansio             | Analytics consumer | JSONL event log, ingested offline into DuckDB                                  |
| 6 | k6 load generator  | Mansio             | IR scenarios over /v1/ops and trace headers                                    |

## Runtime notes

Since IR-6 the server is the Go server in server/; the Deno platform was deleted (ADR 0013). App handlers run
in the client SDK and reach the server as IR requests (intermediate-representation/spec/v1.md); no application
SQL crosses the client boundary. Tenant identity comes from tickets, API keys or sessions (ADR 0012); Postgres
is inside the system, shown at Level 2. No external identity provider is wired.
