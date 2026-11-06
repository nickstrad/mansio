# Local dev infrastructure

`docker-compose.yml` (repo root) runs a single-container [SeaweedFS](https://github.com/seaweedfs/seaweedfs)
(master + volume + filer + S3 API), a local S3-compatible store for the Go server's durable agents and
`dbtool`, and a Postgres 17 for agentd's `api-key` and `session` tenancy modes (its `pg_dsn`). A root
`Makefile` wraps the common actions. Everything here is **dev only**.

## Quickstart

```sh
make up        # start SeaweedFS and Postgres, wait (max 60s) until healthy
make s3-init   # create the mansio-test bucket, print endpoint + credentials
make help      # all targets: up down restart ps logs clean s3-init
```

`make down` stops the containers and keeps the `seaweedfs-data` volume; `make clean` also deletes the volume
(asks `[y/N]` first). `make s3-init` is idempotent and installs the AWS CLI if it is missing.

## Pointing the Go server at it

| Setting    | Value                                                            |
| ---------- | ---------------------------------------------------------------- |
| Endpoint   | `localhost:8333` (plain HTTP)                                    |
| Access key | `mansiodevkey`                                                   |
| Secret key | `mansiodevsecret`                                                |
| Bucket     | `mansio-test` (created by `make s3-init`)                        |
| Postgres   | `postgres://mansio:mansio@localhost:5432/mansio?sslmode=disable` |

The credentials are public test values defined in `docker-compose.yml` (the `seaweedfs-s3` config) — dev-only,
never reuse them anywhere real. agentd's Postgres adapter needs an explicit `sslmode` in the DSN.

- **agentd in durable mode**: `"s3": {"endpoint": "localhost:8333", "bucket": "mansio-test", "prefix": "…/"}`
  in the config, the keys as `s3_access_key` and `s3_secret_key` in its `secrets_file` (mode 0600), and the
  `-dev` flag, which plain-HTTP S3 requires. `tests/e2e/platform/durable.ts` (`startDurableAgent`) writes a
  complete config; field reference: `server/cmd/agentd/config.go`.
- **dbtool**: `-endpoint localhost:8333 -bucket mansio-test -root <the agent's prefix>`, with the keys in
  `DBTOOL_S3_ACCESS_KEY` and `DBTOOL_S3_SECRET_KEY`.
- **Go tests gated on `COLDLOAD_S3=localhost:8333`** (and `MANSIO_PG_URL`) use the identities and bucket of
  `server/deploy/seaweedfs/` (e.g. `coldload`/`coldload-secret`) and the Postgres of
  `server/deploy/postgres/`, not this stack's; start those compose files for them. The comments of each gated
  test name what it needs.

The e2e suite does not use this stack: `work-scripts/bash/e2e-linux.sh` starts its own SeaweedFS and Postgres.

Debug UIs: master <http://localhost:9333>, filer <http://localhost:8888>, volume <http://localhost:8080>.

## Adding another service

Add it under `services:` in `docker-compose.yml` (a commented template is at the bottom of that block): pin
the image tag, map ports, set `deploy.resources.limits`, and give it a `healthcheck` — `make up` waits for
every service that has one. Declare any new named volume under the top-level `volumes:`.
