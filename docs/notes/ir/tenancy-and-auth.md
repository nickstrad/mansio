# Tenancy and auth on agentd

How a single agentd takes API keys, sessions and app tokens as well as tickets on `/v1/ops`, serves the tenant
admin API and sign-in, and how the Deno platform's Postgres tenancy rows are imported. Decision:
[ADR 0012](../../adr/0012-single-node-auth-port.md). The multi-node half (controld authenticates at resolve)
is in [multi-node](multi-node.md).

## The pieces

| File                                                | Role                                                                       |
| --------------------------------------------------- | -------------------------------------------------------------------------- |
| `server/internal/tenancy`                           | `Service`: `Authenticate(token, app)`, admin ops, `SignUp`/`SignIn`, cache |
| `server/internal/tenancy/password.go`               | Better Auth scrypt (golden: two real Better Auth 1.7.7 hashes)             |
| `server/internal/tenancy/tenancyhttp`               | `/admin/` gate + tenants/keys/link routes, `/api/auth` sign-up/sign-in     |
| `server/internal/tenancy/tenancytest`               | `Fake` store (fault switch) and `RunStoreSuite` (fake and Postgres)        |
| `server/internal/placement/pg` `tenancy.go`, `0007` | the store over controld's schema; `tenancy_*.sql`                          |
| `server/internal/placement/pg` `import_mansio.go`   | `ReadMansio` + `ImportMansio` (`import_mansio*.sql`)                       |
| `server/internal/front` `Credentials`, `verifyOps`  | non-ticket bearers on `/v1/ops`; `AuthError` 401                           |
| `server/cmd/agentd/tenancy.go`                      | `tenancy` config section, secrets file, `create_dbs`, background migrate   |
| `server/cmd/controld/import.go`                     | `controld import-mansio [-dry-run]`                                        |
| `tests/e2e/platform/harness.ts`                     | `startGoTenancy`, `startTenancy`, `opsPath`, `opsQuery`; `client` creds    |

## agentd config

```json
{
  "db_dir": "dbs",
  "create_dbs": true,
  "tenancy": { "mode": "api-key", "secrets_file": "tenancy.json", "pg_schema": "control", "cache_ttl": "1s" }
}
```

`create_dbs` needs `schemas_dir` (only registered apps and schemas create a database); caps `max_dbs` and
`max_dbs_per_tenant` answer `QuotaExceeded` 429. `tenancy.apps` defaults to the apps in `schemas_dir`.
`tenancy.json` (0600): `{"admin_token", "app_tokens": {app: token}, "pg_dsn"}`. Give the DSN an explicit
`sslmode` (pgx's default `prefer` adds a fallback config, which the placement adapter refuses). The schema
(`search_path`) must exist; agentd migrates it in the background and `/readyz` waits for that.

## Traps

- A bearer is tried as a ticket first; only `ErrMalformed`/`ErrVersion` (not ticket-shaped) go to the port. A
  real but expired ticket stays `ticket_invalid`.
- Key ids are 16 hex on Go and were uuids on the Deno platform. `mansioctl tenants revoke-key` accepts both;
  the harness `tenant()` reads the key id from the key table's row.
- `/admin/` is one mux pattern on the query listener: a later `/admin/...` route must be registered as a more
  specific pattern (e.g. `POST /admin/logs/ingest`) on the same mux, or inside tenancyhttp's gate.
