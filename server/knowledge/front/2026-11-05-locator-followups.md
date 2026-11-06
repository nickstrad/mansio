# 2026-11-05 — Locator, controld and edge follow-ups (after CP-5..CP-8)

Branch `cp/fin-loc`. Closes deferrals of CP-5 ([locator](2026-11-05-locator.md)), CP-6 ([client](2026-11-05-client.md))
and CP-7 ([edge](2026-11-05-edge.md)).

## 1. The locator answers `moving` until the new owner's claim lands

Found by the CP-6 gate: the locator routed to a move's destination (or a create's first owner) as soon as the
placement flipped, before the owner's Claim; that owner answered `wrong_owner` at the current epoch.

- **Rule** (`locator.claimPending`): placement ACTIVE, and the registry still says `creating`, or `moving` with
  `target_vm` = the placement's owner → `moving` with `locator.claim_retry` (50 ms). `moving` with the placement
  still on the source (not yet fenced) is routed to the source.
- **Hint precision:** controld's 503 answers now carry `retry_after_ms` in the body (the `Retry-After` header stays
  whole seconds, ≥ 1); the client reads it for locator answers, as it already did for agents.
- **Notification:** migration `0005_state_notify.sql`: `pg_notify('database_state', database_id)` after an update
  that changes `state` or `target_vm` (no insert: a new row has no placement and is never cached). `pg.Listen`
  LISTENs on both channels before `Ready` (LISTEN first, then load, as 0003) when `ListenHooks.State` is set;
  `Locator.InvalidateState` tombstones the key whatever its epoch (the flip does not change the epoch). `locate.sql`
  returns `target_vm`.
- **Client:** the "primary answer ends min_epoch" rule stays (an agent restart, a lost notification or an edge
  that does not know a VM yet still give `wrong_owner` at the current epoch); only its comment changed.
  `MaxReroutes` stays 5 (a move now costs ~2: `fenced`, then `moving`).
- **Cost of a stuck flow:** if the registry flip to active fails after the claim, resolves answer `moving` until
  the resumer (`resume_after`, 30 s) finishes it. Before, they routed to an owner that served.

`TestClientGate` (frontbench locator, conc 16, 4 dbs, 30 % writes, a move per second for 8 s; M5 Max, PG 17 +
SeaweedFS in Docker): 165k requests, **0 failed** (124k overlapped a move), resolve misses 65 (was 102–116),
reroutes 92 (was 123–135), controld primary/replica loads 24. `TestLocatorGate` passes unchanged.

## 2. TLS for controld's API

`tls: {cert_file, key_file}` (h2 + h1, `tlsx.ServerConfig`); without it controld refuses to start unless `-dev`
(admin tokens, API keys and tickets cross this listener; whoever answers `GET /v1/keys` can mint tickets). CLI
`-ca FILE`. agentd and the edge: `control_ca_file` (the CA of controld's certificate) for an https `keys_url`
(and the edge's `registry.url`); `keyfetch.Config.CAFile`. Shared: `tlsx.Files`, `tlsx.HTTPClient(caFile)`.
Test: `cmd/controld` `TestListenerTLS` (no Postgres needed), `cmd/agentd` `TestKeyFetcherTLS`.

## 3. Unknown API key ids are rate-limited per source

`edge.SourceLimiter` moved to `internal/ratelimit` (edge and controld share it). The locator charges
`Request.Source` (`ratelimit.SourceKey(RemoteAddr)`) one token for each key id **not in its key cache**, before
the store read; a known id's refresh is free. Empty bucket → `shed` 503 + Retry-After (quiet; counted in
`controld_locator_rate_limited_total`). Config `locator.rate_limit` (`lookups_per_second` 20, `burst` 50,
`max_sources` 100k). The client retries a locator `shed` like `unavailable`. Gate: 40 garbage ids from one source
with burst 5 → 3 reached Postgres, 37 shed; the cached key from the same source still resolved.

## 4. Edge routes from controld's VM registry

controld `GET /v1/agents` (Bearer `edge_token`, a new optional secret; off without it): every VM not attested
dead → its query endpoint (`vm_load.sql`, ≤ 10k). `edge.RegistryRoutes` (an `edge.Resolver`): answers from the last
map; polls every `registry.refresh` (10 s); a verified ticket naming an unknown VM asks for an early fetch (at most
once per second), so a new VM costs ~1 s of `wrong_owner`; deadline (`registry.timeout`) and a breaker per fetch; a
failed or invalid answer keeps the last map. Config `registry: {url, token_file (0600), refresh, timeout,
agent_scheme}`, exclusive with static `agents` (still supported). Metrics `edge_registry_agents`,
`edge_registry_fetch_failures_total`, `edge_registry_age_seconds`. Example `configs/edge/local-controld.json`.
Tests: `internal/edge` `TestRegistryRoutes*`, `cmd/edge` `TestRegistryRoutes` (TLS both hops, new VM learned,
failing registry keeps routes), `TestLocatorGate` (`/v1/agents` with edge vs admin token).

## 5. Cleanups

- `internal/fsx`: `WriteAtomic(path, data, perm)` + `SyncDir` replace four copies (owner file store, file
  placement, controld key dir, keyfetch cache). The key cache now fsyncs its directory.
- `logx.Reporter` / `logx.Open` / `logx.Levels` replace controld's and the edge's reporter and openLog; agentd
  keeps its copy (out of scope here) and can adopt them as is.

## Still open

mTLS edge → agent; edge admin controls; edge on separate hosts (numbers); agentd adopting `logx.Reporter`.
