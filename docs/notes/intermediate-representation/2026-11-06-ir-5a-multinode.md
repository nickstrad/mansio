# 2026-11-06 — IR-5a: multi-node IR (controld auth at resolve, the edge on `/v1/ops`, `scale_out`, Jepsen run)

Question: how does the SDK reach a database on a cluster (controld, two durable agents, the edge), who
authenticates which credential, and what do the system tests show? Built on `ir/05a-multinode` from `main` at
`8124c2a` (main merged at `99c7c2f`). Plan: `intermediate-representation-protocol.md` (removed; see git
history) §5 IR-5, §6.5; `replace-platform-with-go.md` (removed; see git history) §5 phase 6. DST-2/DST-3 are a
later unit.

## The pieces

| File                                                | Role                                                                                    |
| --------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `server/internal/control/locator` `Credentials`     | port: bearers that are not new API keys (legacy keys, sessions) for app resolves        |
| `server/cmd/controld/tenancy.go`                    | `tenancy` section: the `internal/tenancy` service, tenancyhttp, create caps             |
| `server/internal/tenancy` `AnyApp`, `Cached`        | controld checks the app itself; the locator charges its limiter only on uncached tokens |
| `server/internal/placement/pg` `create_api_key.sql` | refuses a deactivated tenant (`tenancy.ErrDeactivated` → 409); `count_databases.sql`    |
| `server/internal/edge` proxy                        | forwards `POST /v1/ops` too (trace/handler headers), own answers in the IR envelope     |
| `sdk/transport.ts` `LocatorOptions.edge`            | every request to the edge (it routes by the ticket's vm), not to the answered endpoint  |
| `server/internal/front` `OpsConfig.CredentialPlace` | a credential request's epoch/gen from a callback (single-node durable tenancy)          |
| `server/cmd/agentd/tenancy.go` `claimingPlace`      | durable + placement_dir: claim, or create (create_dbs, capped) then claim               |
| `tests/e2e/platform/cluster.ts`, `scale_out.ts`     | the cluster harness and the scenario (Go only)                                          |
| `server/internal/linearize`                         | Wing–Gong–Lowe linearizability checker (generic model, unknown outcomes, partitions)    |
| `server/cmd/controld/jepsen_test.go`                | the Jepsen-style run (gated `MANSIO_JEPSEN=1`)                                          |
| `server/cmd/controld/{tenancy,import}_gate_test.go` | auth at resolve; `dbtool import` → Postgres placement → create adopts                   |

## Auth at resolve (ADR 0012, multi-node half)

`POST /v1/resolve {["tenant",] "app"[, "create"][, "min_epoch"]}`. A bearer that parses as a new API key
(`mansio_<16 hex>_<secret>`) takes the locator's own path (row cache, perms, principal; `api_key.sql` reads a
deactivated tenant's keys as revoked). Any other bearer, on an **app** resolve and with a `tenancy` section
(`{"mode": "api-key" | "session", "apps"?, "cache_ttl"?, "session_ttl"?}`), goes to `tenancy.Service` (legacy
keys by hash, sessions by hash, constant-time compares, read+write, ticket `sub` `key:legacy` or `session`). A
raw `{"tenant","db"}` resolve stays API-key-only. The tenant comes from the credential: an omitted `tenant`
takes it, another is 403. An uncached credential costs its source a limiter token first (as an unknown key id
does). The section also mounts tenancyhttp on controld (`/admin/tenants…` under the admin token, `/api/auth/*`
in session mode) over the same schema. Off mode (app tokens) is refused on controld.

Create on first use: `locator.ErrCreatable` now carries the credential's tenant in the route; `resolveApp`
checks `locator.max_dbs_per_tenant` (100) and `locator.max_dbs` (10 000) over live registry rows
(`count_databases.sql`) → 429 `quota_exceeded`, then the registered-schema check, then `Controller.Create`.
The cap check is not atomic with the create (concurrent first uses may pass it by up to `locator.concurrency`
× controld instances); explicit creates are not capped. `tenancy.apps`, when set, limits every credential's
app resolves (404).

## Create and move for (tenant, app)

Nothing new was needed: `(tenant, app)` is the database `tenant:app`, so `POST /v1/databases/{key}/move`,
attest-dead and restore work on it as on any database. `scale_out` and the Jepsen run exercise them.

## Single-node durable tenancy

agentd accepts `tenancy` and `create_dbs` in durable mode with `placement_dir` (refused with `placement_pg`:
controld's job). A credential request runs at the owner table's epoch; a database not active here is claimed
(e.g. after `dbtool import -placement-dir`), or with `create_dbs` the claim path writes `ACTIVE(self, 1, 0)`
first (caps: the owner table's keys). Claims/creates are serialized; warm requests take no lock.

## `scale_out` (e2e, Go only; skipped on Deno with a reason)

Cluster: controld (Postgres schema `control` in the scenario database, tenancy api-key, ticket TTL 60 s), two
durable agents (SeaweedFS, `keys_url` and `schemas_url` at controld, write-back 200 ms), the edge (registry
from controld). The SDK resolves with an API key from controld's `/admin/tenants`, create on first use, and
sends everything through the edge. Phases: (1) four paced writers while the database moves vm-a → vm-b → vm-a:
no definite failure, writes acknowledged during each move, every acknowledged `createGame` stored exactly once
and nothing unacknowledged stored (checked over all games); (2) controld stopped: 20 writes succeed on cached
routes, tickets, edge registry and keys; (3) owner written back (`/admin/backup`), kill -9, restore refused
before attestation (409), attest-dead + restore moves it to the live VM, every acknowledged write is served
and the same client writes again once the edge's registry drops the dead VM. About 6 s.

## Jepsen-style run

```sh
MANSIO_JEPSEN=1 MANSIO_JEPSEN_DURATION=2m MANSIO_PG_URL='postgres://mansio:mansio-dev@localhost:5433/mansio?sslmode=disable' \
  COLDLOAD_S3=localhost:8333 go test -count=1 -run TestJepsen -v -timeout 20m ./cmd/controld
```

Topology: faultnet on client→edge, {clients, agents, edge}→controld, edge→each agent's query listener,
controld→each agent's lifecycle listener. Nemesis (one at a time, every 2–4 s, 1–4 s each): proxy faults
(latency, refuse, silence, frozen, reset), kill -9 of an agent and restart on the same files/ports, a move,
`docker pause` of `mansio-postgres` / `coldload-seaweedfs`. Six clients send raw IR through the edge with
tickets from resolve (same request id on every retry; SDK reroute/retry rules). Checks: each register's
history and the bank's history linearizable (`internal/linearize`), every full bank read sums to the total.

**Finding: kill -9 rolls back acknowledged writes by design.** A restarted agent serves each database from its
last published generation, so writes acknowledged inside the write-back window are lost (the RPO; DST-1's RPO
rule, `knowledge/writeback`). The first unsegmented runs caught exactly that (a register read 6954 after 6999
was acknowledged and read; ~170 ms of writes, the 200 ms window). The checker therefore splits each database's
history at the crashes of the agent that owned it then (read from the authority just before each kill): within
a segment it must be linearizable; the first segment starts from the setup state; a later one starts unknown,
but its first value of each register (or (balance, version) of each account) must be the setup's or one
written by an operation called before the crash — a restart rolls back to an earlier state, never to a value
nobody wrote. Operations in flight across a crash are dropped (reads) or optional (writes). The run logs a
per-crash estimate of acknowledged register writes lost. Conservation is checked across crashes (a rollback is
to a consistent generation). Runs: 40–45 s, 2 min and 2.5 min (7 kills, 5 per database's owner; lost per owner
crash ≈ 40, 12, 14, 35, 0): linearizable per segment, conserved; the stronger start-state rule caught nothing
new. How long the RPO may be is an open decision
(`docs/plans/completed/intermediate-representation-protocol.md` §9).

## Traps

- The edge's own answers on `/v1/ops` must use the IR envelope (`{"error":{"tag":…}}`): the SDK treats a
  `front.Response` body as an untagged transport failure. Unknown VM → 421 `wrong_owner` (re-resolve).
- In the TS harness, never `await` process diagnostics while the process runs (`child.output()` resolves at
  exit): a template literal evaluated eagerly in an `assert.ok` message hung the first runs.
- A test that edits files in the worktree while `e2e-linux.sh` runs fails the hermeticity check
  (`git status changed`).
