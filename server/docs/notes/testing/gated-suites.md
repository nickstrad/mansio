# Running the Postgres- and S3-gated Go suites

How to run the controld and placement tests that need a real Postgres and a real S3 store.

`cmd/controld`'s gates (`TestGate`, `TestClientGate`, `TestLocatorGate`, …) and `internal/placement/pg` skip
unless `MANSIO_PG_URL` and `COLDLOAD_S3` are set; `deno task go:ci` therefore never runs them. They caught a
real regression once (charging the locator's `moving` to the retry budget; see
[the retry contract](../backpressure/retry-contract.md)), so run them when controld, the locator or a client's
retry loop changes.

When the default ports (Postgres 5432, S3 8333) are taken by another stack on the machine, use throwaway
containers on other ports (checked 2026-11-10 on the dev VM: all gates pass this way):

```sh
cd server
docker run -d --name bp-pg -e POSTGRES_USER=mansio -e POSTGRES_PASSWORD=mansio-dev -e POSTGRES_DB=mansio \
  -p 127.0.0.1:25432:5432 postgres:17 postgres -c max_connections=300
docker run -d --name bp-seaweed -p 127.0.0.1:28333:8333 \
  -v "$PWD/deploy/seaweedfs/s3.json:/etc/seaweedfs/s3.json:ro" chrislusf/seaweedfs:latest \
  server -dir=/data -ip.bind=0.0.0.0 -master.volumeSizeLimitMB=1024 -volume.max=40 -s3 -s3.port=8333 \
  -s3.config=/etc/seaweedfs/s3.json
# agentd's scoped S3 identity cannot create the bucket; without it every gate fails with
# "claim ... answered 503 unavailable" (it looks like a product bug and is not):
docker exec bp-seaweed sh -c 'echo "s3.bucket.create -name coldload" | weed shell'
MANSIO_PG_URL='postgres://mansio:mansio-dev@localhost:25432/mansio?sslmode=disable' COLDLOAD_S3=localhost:28333 \
  go test -count=1 ./cmd/controld/ ./internal/placement/pg/      # about 90 s
docker rm -f bp-pg bp-seaweed
```

Each gate uses its own Postgres schema and S3 prefix and drops them, so the containers can be reused between
runs.
