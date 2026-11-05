# 2026-11-05 — Scoped S3 identities, TLS and -dev (OS-11)

Author: CP-4 agent-side worker (branch `cp/cp4-agentside`). Plan: object-storage plan OS-11, control-plane plan
CP-4. Guide rules: object-storage S1 (one credential per component, scoped), S2 (TLS outside a laptop), K (no
LIST on a request path), X12.

## Identities (`deploy/seaweedfs/s3.json`)

Throwaway dev keys; the same table is in the compose file's comment. Never use them outside this container.

| Identity | Keys | SeaweedFS actions | Used by |
|---|---|---|---|
| `coldload` | `coldload` / `coldload-secret` | Admin, Read, List, Tagging, Write (unchanged) | coldload, benches, every existing test and tool, test setup and cleanup |
| `agent` | `agent` / `agent-secret-dev` | `Read:coldload`, `Write:coldload`, `Read:tsdb`, `Write:tsdb` | agentd (the gated real-S3 agentd tests now run with it) |
| `gc` | `gc` / `gc-secret-dev` | Read, List, Write on `coldload` and `tsdb` | the orphan sweeper, `dbtool` |
| `bench` | `bench` / `bench-secret-dev` | Admin (unscoped: it creates buckets) | dev benches only |

The agent's buckets are `coldload` (what the tests use) and `tsdb` (`configs/agentd/durable-local.json`).

## What the agent identity can do (SeaweedFS 4.47, single node, measured 2026-11-05)

`COLDLOAD_S3=localhost:8333 go test -count=1 -v -run TestS3Compat ./internal/objstore` → X12 pass:

| Request | agent |
|---|---|
| HEAD bucket (`BucketExists`, minio's `?location` lookup) | allowed (Read) |
| PUT, HEAD, GET object; conditional PUT; metadata | allowed |
| DELETE, batch DELETE (`RemoveObjects`, POST `?delete`) | allowed (Write) — needed: owner-side retention deletes old generations by name |
| LIST (`ListObjectsV2`) | **AccessDenied** (403) → `objstore.Classify` = `Permanent` |
| create bucket | **AccessDenied** |
| PUT into a bucket outside its scope | **AccessDenied** |

`gc` can LIST; `bench` can create buckets. X12 now defaults to the dev agent keys (`S3_AGENT_ACCESS_KEY` /
`S3_AGENT_SECRET_KEY` override; `-` skips; a backend without the identity records a skip) and blocks when the
identity exists.

## Does agentd need anything more? No.

- **No agent code path lists.** LIST is in `objstore.List*`/`EachPage`/`DeletePrefix` and `Durable.ListKey(s)`,
  used only by `durable` GC (the sweeper) and `dbtool`; agentd runs neither. Retention (`vactor.retain`)
  deletes by computed name.
- **Startup's `EnsureBucket`** does a HEAD bucket first, which the agent may do, and only calls `MakeBucket`
  when the bucket is missing. **Decision: creating the bucket is not the agent's job** (it is setup's: the
  admin/`bench` identity, or controld's provisioning later). With the agent identity and no bucket, agentd
  still starts and logs `S3.BUCKET_CHECK_FAILED` (WARN), exactly as when S3 is down; nothing activates until
  the bucket exists. No code change was needed.
- Evidence: the gated agentd e2e (`COLDLOAD_S3=localhost:8333 go test -run TestDurableAgainstRealS3
  ./cmd/agentd`: kill -9, lifecycle, restart, SIGTERM) passes with the agent identity, and the lifecycle test
  fails if the log shows `AccessDenied` or `S3.BUCKET_CHECK_FAILED`. The rig ensures the bucket and cleans its
  prefix with the admin identity.

## TLS and -dev

- `objstore.Config{Secure, CAFile}`: HTTPS (TLS ≥ 1.2, 5 s handshake timeout); `CAFile` is a PEM bundle put in
  the transport's `RootCAs`. Validated in `objstore.New`: a CA file without `Secure`, an unreadable file, or a
  file with no PEM certificate is an error. Tested in-process with `s3test.NewTLS()` (+ `CAPEM()`).
- A certificate the client cannot verify stays `Transient` in `Classify` (retried, counts for the breaker):
  from the agent's side an expired or rotated server certificate is a dependency outage → `unavailable`, never
  `internal`.
- agentd: `s3.secure`, `s3.ca_file` (relative to the config file; `ca_file` without `secure` is a config
  error). **agentd refuses durable mode with `s3.secure: false` unless `-dev` is given** (exit 1:
  `s3.secure is false: plain-HTTP S3 is for local development only; set "secure": true (and "ca_file" for a
  private CA) in the s3 section, or pass -dev for a local run`). `-check` applies the same rule. Static mode
  has no S3 and needs nothing.
- `-dev` also gates CP-1's `claim {"create": true}` stand-in: without `-dev` it answers `400 bad_request`
  ("create is the control plane's job (controld); -dev only"). controld writes the placement row itself and
  then sends a plain claim (`TestDurableOverTLSWithoutDev` does exactly that over TLS with a private CA).
- Local SeaweedFS is plain HTTP, so local runs and the durable-mode tests pass `-dev`.

## Operational note

`docker compose -f deploy/seaweedfs/docker-compose.yml up -d --force-recreate` reloads `s3.json`; the data is in
the named volume `seaweedfs_seaweed-data` and survives. The compose file mounts `./s3.json` relative to itself,
so recreate from the checkout whose `s3.json` you want live.
