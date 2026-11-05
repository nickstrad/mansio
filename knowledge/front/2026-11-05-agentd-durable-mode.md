# 2026-11-05 — agentd durable mode: the CP-1 wiring

Author: CP-1 agent. Plan: [control-plane plan CP-1](../../docs/control-plane-plan.md). Code: `cmd/agentd`
(`durable.go`, `glue.go`, `control.go`, `config.go`), `internal/placement/file` (passes CP-3's `ownertest.RunPlacementSuite`).
Tests: `cmd/agentd/glue_test.go`, `durable_test.go` (gates), `durable_config_test.go`, `internal/placement/file`.

## What runs

An `s3` section in the config turns on durable mode (no `s3`: static mode, the old `db_dir` server, kept for
frontbench and `scripts/front/bench.sh`).

```
front ── depAdmitter{ownerAdmitter{owner.Table}} ── depExecutor{hostExecutor{vactor.Host}} ── ActorExecutor
                         │                                        │ GetAt(db, ticket epoch)
                         │ Claim/Fence/Reconcile                  │ CheckActivation → Table.CheckActivation
                         ▼                                        │ Guard (ticks)   → Table.Publish
            hostWriteBack (Publish→WriteBackAt, Head→ReadManifest, Stamp→host.Stamp)
                         │
     durable.WithBreaker(objstore.NewDurable(store, prefix), s3 breaker)   placement/file (flock CAS)
```

- One `objstore.Store` per process; the S3 breaker uses `RI §P` defaults, overridable as `s3.breaker`.
- Startup: `owner.Open` (FileStore) → serve → `ReconcileUntilDone` (workers = `startup_workers`, per key
  `reconcile_per_key`) in the background. Readiness = first reconcile round done + canary. S3 bucket check
  failing at startup is a WARN (`S3.BUCKET_CHECK_FAILED`), not a refusal to start.
- Shutdown phase `host.close` = `vhost.Shutdown` (oldest-dirty first, rest kept; `VACTOR.SHUTDOWN` logs
  written_back / kept), then the canary.
- Metrics added in durable mode: `tsdb_s3_*`, `tsdb_breaker_*{dep="s3"}`, `tsdb_vactor_*`,
  `tsdb_owner_transitions_total`, `tsdb_s3_probe_{ok,failures_total,last_ok_timestamp_seconds}` (a manifest
  read past the breaker every `ready_interval`; never fails readiness). `tsdb_startup_pending` = keys still
  closed. Not in durable mode: `tsdb_actor_queue_depth` (vactor exposes no per-actor queues; deferred).

## Lifecycle endpoints (control-plane identity: `Authorization: Bearer <control_token>`)

`POST /v1/lifecycle/{claim,fence,forget}` `{"db": ...}`, `GET /v1/lifecycle/health`; on the lifecycle listener;
each under `lifecycle_timeout` (30 s; `X-Timeout-Ms` may shorten it), at most `lifecycle_concurrency` (16) at
once, the rest 503 at once. Codes: `bad_request` 400, `unauthorized` 401, `wrong_owner`/`moving`/`fenced`/
`illegal` 409, `unavailable` 503 + Retry-After, `timeout` 504, `internal` 500 + log ID (table in
`control.go`). Fence drops the warm copy after the ack (and on supersede). `"create": true` on claim writes
ACTIVE(self, 1, 0) when no placement row exists: the single-node stand-in for controld's flow B, to delete at
CP-4.

## Secrets

`secrets_file` (required in durable mode): `{"s3_access_key", "s3_secret_key", "control_token"}`; refused if
group/other bits are set, if a field is unknown or missing, or if the token is < 16 characters. Errors never
echo a value.

## Decisions

- **Static mode stays** beside durable mode: the benchmarks need a server without S3 or a control plane.
- **File placement shares one authority across processes** (flock on `<dir>/.lock`, polled so the wait honors
  ctx): the gate's operator step (an M5 epoch bump while the agent is down) is a CAS from the test process.
- **A cold key's fence acknowledges the manifest's generation** (`hostWriteBack.Publish`): nothing unpublished
  can exist for a key that is not warm (passivation writes back first; restarts empty the cache).
- **Warm copies of keys no longer owned are discarded** by the tick guard (fenced/absent/superseded only).
- **Claim of a key with no placement row answers `unavailable`** (owner.Table wraps any authority error so);
  with `create` it is created first.

## Gate results (M5 Max, s3test in-process and SeaweedFS 4.47 single node)

| Gate | Result |
|---|---|
| e2e claim → write → passivate → kill -9 → restart → reconcile → read | all 11 written-back rows back; 5 unpublished rows lost (inside the RPO); same request id after restart and after the epoch bump → `ok` + `duplicate` |
| old epoch after M5 bump | `fenced` before the claim (superseded on reconcile), `wrong_owner` after the claim at epoch 2 |
| time to first served key < all reconciled | 0.42 s vs 2.41 s (12 keys, 1 worker, 40 ms per S3 exchange); 0.47 s vs 2.76 s on SeaweedFS |
| S3 refuse / blackhole | cold → 503 unavailable + Retry-After; warm reads and writes ok; /readyz 200; breaker open on /metrics; fail fast while open; recovers after heal |
| ApplyThenDropReply on the fence's two PUTs | resolved by re-read; ack = stored generation |
| SIGTERM, 6 dirty keys, 3 s budget | healthy: exit 0 in ~1.4 s, all written back and read back after restart; black-holed: exit 1 in ~2.4 s, kept copies reported |
| `TestBucketBytesFlatThroughS3`, `TestS3Compat` | still pass on SeaweedFS |
