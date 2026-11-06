# Local dev infrastructure

`docker-compose.yml` (repo root) runs a single-container [SeaweedFS](https://github.com/seaweedfs/seaweedfs)
(master + volume + filer + S3 API) so the platform's ObjectStore S3 layer has a local S3-compatible backend. A
root `Makefile` wraps the common actions. Everything here is **dev only**.

## Quickstart

```sh
make up        # start SeaweedFS, wait (max 60s) until healthy
make s3-init   # create the mansio-test bucket, print endpoint + credentials
make help      # all targets: up down restart ps logs clean s3-init
```

`make down` stops the containers and keeps the `seaweedfs-data` volume; `make clean` also deletes the volume
(asks `[y/N]` first). `make s3-init` is idempotent and installs the AWS CLI if it is missing.

## Pointing the platform at it

| Variable                | Value                   |
| ----------------------- | ----------------------- |
| `S3_BASE_URL`           | `http://localhost:8333` |
| `AWS_ACCESS_KEY_ID`     | `mansiodevkey`          |
| `AWS_SECRET_ACCESS_KEY` | `mansiodevsecret`       |
| `AWS_REGION`            | `us-east-1`             |

The credentials are public test values defined in `docker-compose.yml` (the `seaweedfs-s3` config) — dev-only,
never reuse them anywhere real. Bucket: `mansio-test`.

Debug UIs: master <http://localhost:9333>, filer <http://localhost:8888>, volume <http://localhost:8080>.

## Adding another service

Add it under `services:` in `docker-compose.yml` (a commented template is at the bottom of that block): pin
the image tag, map ports, set `deploy.resources.limits`, and give it a `healthcheck` — `make up` waits for
every service that has one. Declare any new named volume under the top-level `volumes:`.
