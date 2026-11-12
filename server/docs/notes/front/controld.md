# controld: registries, placement policy and lifecycle

The control-plane daemon: its Postgres registries, the placement policy, the create/move/suspend/drop/restore
flows, and the gate results.

Code: `cmd/controld`, `internal/control` (domain: policy + controller), `internal/control/agenthttp` (the
agents' lifecycle client), `internal/placement/pg` (`schema/0002_registry.sql` and the registry queries),
`cmd/agentd` (`placement_pg`). agentd's runtime controls: [admin controls](admin-controls.md)
(`cmd/agentd/admin.go`, `cmd/mansioctl`); S3 identities:
[S3 identities and TLS](../writeback/s3-identities-and-tls.md).

## What runs

```
operator / script ── controld <cmd> -addr -secrets ──► controld API (admin_token)
                                                          │ internal/control.Controller
        ┌─────────────────────────────────────────────────┼──────────────────────────────┐
        ▼                                                 ▼                              ▼
  Postgres (placement/pg): placement, moves,     agents' lifecycle endpoints      S3 (gc identity):
  vms (+bytes), tenants, databases               claim / fence / forget / health   DeletePrefix(KeyPrefix(key))
  (one pool, one "placement" breaker)            (agenthttp: deadline + breaker
                                                  per agent, no retries)
background: heartbeat poller (health → vms.heartbeat, bytes), resumer (moving.sql + pending.sql, every resume_interval)
```

agentd gains `placement_pg` (DSN `pg_dsn` in the secrets file): the owner table reads the same Postgres
authority; `pg.ErrUnavailable` answers `unavailable`; its breaker is `"placement"` on `/metrics` and on the
admin endpoints. `placement_dir` (file) stays for one-host runs; exactly one of the two is required.

## Registries (migration 0002)

- `tenants(tenant_id)`;
  `databases(database_id = tenant ':' name, tenant_id, name, is_default, state, target_vm)`; `vms.bytes` (the
  agent's last reported cache bytes).
- Database states: `creating → active ⇄ moving`, `active → suspended → moving → active`,
  `{active, suspended, creating} → dropping → dropped`. `target_vm` is set iff `moving` (a check).
- The tenant's first live database is its default (one per tenant: a partial unique index).
- A dropped row stays as a tombstone: **names are never reused** (a new database under an old key would meet
  the old one's epochs and leftover objects).
- Every constraint has its off point in `schema/test/constraints.sql` (golden).

## Placement policy

`control.Policy.Pick`: among VMs not attested dead whose heartbeat is younger than `max_heartbeat_age`
(default 3 × `heartbeat_interval`) and that are under both caps (`max_databases`, `max_bytes`; 0 = no cap),
pick the smallest **dominant share** `max(dbs/max_databases, bytes/max_bytes)`, then fewer databases, then
fewer bytes, then the VM id. Deterministic, no randomness. Databases are the primary measure (a warm database
costs memory, handles and maintenance whatever its size); bytes catch the few large ones. Racing creates are
safe through the authority's CAS; they may only skew the spread slightly (gate: 47–53 per VM for 100 creates 8
at a time).

## Flows (all resumable; the resumer is the one retry layer)

| Flow                    | Steps                                                                                                                                            | Resumed from                                                                            |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------- |
| Create                  | registry `creating` → pick → CAS(absent → ACTIVE(vm, 1, 0)) → `Claim` → `active`                                                                 | `pending.sql` (`creating`)                                                              |
| Move                    | `moving` + target → `owner.Mover` (MOVING, fence, flip E+1 with the ack's gen, claim) → `active`                                                 | `moving.sql` (row MOVING) or `pending.sql` (flip done, claim failed: the row is ACTIVE) |
| Suspend / Resume        | `suspended` → CAS ACTIVE → MOVING → fence; resume = a move from there                                                                            | repeat `suspend` (the resumer skips suspended rows)                                     |
| Drop                    | `dropping` (tombstone) → MOVING → fence → delete `KeyPrefix(key)` → `Forget` → delete placement row + its moves rows (one statement) → `dropped` | `pending.sql` (`dropping`)                                                              |
| Restore after a dead VM | operator `attest-dead` → `restore`: every row the dead VM owns moves to a policy pick                                                            | run `restore` again                                                                     |

**Fence of a dead VM.** When the registry says a VM is attested dead, the controller's mover answers its fence
from the authority's own row (`Ack{row.Epoch, row.Gen}`) without calling it. The destination's Claim starts
from whatever generation storage holds (≥ the row's), stamping E+1 so a zombie's later publish fails
`If-Match`/epoch (every write is conditional, so a stale writer cannot overwrite). The dead VM's unpublished
writes are lost: the RPO the attestation accepts. For restores, `moves.ack_generation` is therefore a lower
bound, not the generation the destination started from.

## Decisions

- **Create stand-in:** agentd's `claim {"create": true}` stays only behind agentd `-dev` (tests, one-host
  runs); controld never uses it.
- **VM registry source:** controld's config lists the agents (vm, query endpoint, lifecycle URL) and registers
  them at start; liveness is controld polling each agent's health (heartbeat + bytes), so agents need no
  Postgres write access beyond placement CAS.
- **Breaker outcomes for agents:** transport errors, 500 and 504 count as failures; 503 (`unavailable`: the
  agent answered, its own dependency is down) is ignored; 4xx answers are successes (a breaker counts only
  failures that say the dependency is unhealthy).
- **No move de-duplication lock:** one controld is assumed; two would be safe (every step is a CAS or
  idempotent) but waste work ([Postgres placement](../fencing/postgres-placement.md)).
- **TLS:** controld, like agentd, refuses `s3.secure: false` without `-dev` (it holds the gc keys).

## Gate results (2026-11-05, M5 Max, Postgres 17 in Docker, SeaweedFS 4.47 single node; `TestGate`, `-race`, 3 runs)

Agents run with the scoped `agent` S3 identity (no LIST), controld with `gc`.

| Gate                                                                                            | Result                                                                                                                                                                                   |
| ----------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 100 creates across two agentd processes (8 concurrent)                                          | 0.7–0.9 s; spread 47/53, 50/50, 51/49                                                                                                                                                    |
| Move under write load, request ids retried after every non-ok answer                            | move 28–33 ms; 870–2 750 writes acknowledged, every one present exactly once (no lost, no duplicate)                                                                                     |
| kill -9 one agent; `restore` before attestation                                                 | refused (`conflict`, exit 1)                                                                                                                                                             |
| `attest-dead` then `restore`                                                                    | 47–53 databases moved in 0.5–0.85 s; rows written back before the kill readable on the survivor; the moved database's acknowledged writes all present; nothing new placed on the dead VM |
| Drop                                                                                            | tombstone, objects deleted, placement row gone, the name refused afterwards (409)                                                                                                        |
| Runtime `fetch:limit`, S3 breaker trip, scoped S3 identity (`TestS3Compat` X12) and TLS refusal | see `cmd/agentd/admin_test.go`, `internal/objstore` `TestS3Compat` X12, `cmd/agentd` and `cmd/controld` TLS refusal tests                                                                |

## Not implemented

- The resumer does not re-fence suspended databases (a crashed suspend is finished by calling it again).
- The gate resolves through Postgres directly, not through the [locator](locator.md).
- controld's own readiness is "schema migrated and agents registered"; its Postgres breaker is metrics only.
