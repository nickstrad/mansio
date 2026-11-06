#!/bin/sh
# Run the e2e suite (`deno task e2e`, the Go server's conformance suite) for this checkout inside Linux.
#
# Why: the suite needs Linux (/proc process groups, setsid) and a Postgres. This starts postgres:17 as a
# sibling container on a private network (E2E_POSTGRES_URL, so main.ts starts no Compose project),
# cross-compiles agentd, dbtool, controld and the edge for Linux on the host (AGENTD_BIN, DBTOOL_BIN,
# CONTROLD_BIN, EDGE_BIN; the container has no Go), starts a SeaweedFS sibling (E2E_S3_*) for the durable
# agents of backup_restore (tests/e2e/platform/durable.ts) and scale_out's cluster
# (tests/e2e/platform/cluster.ts), and runs the suite in denoland/deno:2.9.5 with the checkout and its git
# directory mounted at their host paths (main.ts checks `git status` before and after).
#
# usage: work-scripts/bash/e2e-linux.sh [go] [deno test args, e.g. --filter documents]
# (`go`, the only backend since IR-6, is accepted for old command lines.)
set -eu
case ${1:-} in
go) shift ;;
deno) echo "BACKEND=deno is gone (IR-6): the e2e suite runs on the Go server only" >&2; exit 2 ;;
esac
root=$(git rev-parse --show-toplevel)
common=$(cd "$root" && cd "$(git rev-parse --git-common-dir)" && pwd)
tag=$(basename "$root" | tr -c 'A-Za-z0-9_.\n-' '-')
id=$$
net=mansio-e2e-net-$id
pg=mansio-e2e-pg-$id
s3=mansio-e2e-s3-$id
bin=$(mktemp -d)
cleanup() {
  docker rm -f "$pg" "$s3" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$bin"
}
trap cleanup EXIT INT TERM

case $(uname -m) in arm64 | aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
(cd "$root/server" && for c in agentd dbtool controld edge; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$bin/$c" ./cmd/$c || exit 1
done)

docker network create "$net" >/dev/null
docker run -d --name "$pg" --network "$net" --network-alias pg --tmpfs /var/lib/postgresql/data \
  -e POSTGRES_USER=mansio -e POSTGRES_PASSWORD=mansio -e POSTGRES_DB=postgres postgres:17 >/dev/null
i=0
until docker exec "$pg" pg_isready -U mansio -d postgres >/dev/null 2>&1; do
  i=$((i + 1))
  [ $i -lt 120 ] || { echo "postgres did not start" >&2; exit 1; }
  sleep 0.5
done

# S3 for the durable agents: SeaweedFS with one dev identity (public test values).
cat >"$bin/s3.json" <<'JSON'
{"identities":[{"name":"e2e","credentials":[{"accessKey":"e2e","secretKey":"e2e-secret"}],"actions":["Admin","Read","List","Tagging","Write"]}]}
JSON
docker run -d --name "$s3" --network "$net" --network-alias s3 -v "$bin/s3.json":/etc/seaweedfs/s3.json:ro \
  chrislusf/seaweedfs:4.48 server -dir=/data -ip.bind=0.0.0.0 -master.volumeSizeLimitMB=64 -volume.max=20 \
  -s3 -s3.config=/etc/seaweedfs/s3.json >/dev/null
i=0
until docker exec "$s3" wget -q -O /dev/null http://localhost:8333/healthz >/dev/null 2>&1; do
  i=$((i + 1))
  [ $i -lt 240 ] || { echo "seaweedfs did not start" >&2; exit 1; }
  sleep 0.5
done
s3env="-e E2E_S3_ENDPOINT=s3:8333 -e E2E_S3_BUCKET=e2e -e E2E_S3_ACCESS_KEY=e2e -e E2E_S3_SECRET_KEY=e2e-secret"

# shellcheck disable=SC2086 # $s3env is a list of flags
docker run --rm --network "$net" -v "$root":"$root" -v "$common":"$common" \
  -v "mansio-nm-$tag":"$root/node_modules" -v mansio-deno-dir:/deno-dir -v "$bin":/e2e-bin \
  -e DENO_DIR=/deno-dir -e AGENTD_BIN=/e2e-bin/agentd -e DBTOOL_BIN=/e2e-bin/dbtool \
  -e CONTROLD_BIN=/e2e-bin/controld -e EDGE_BIN=/e2e-bin/edge $s3env \
  -e E2E_POSTGRES_URL=postgres://mansio:mansio@pg:5432/postgres -w "$root" denoland/deno:2.9.5 \
  sh -c 'apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq git procps >/dev/null 2>&1;
    git config --global --add safe.directory "*"; deno task e2e "$@"' sh "$@"
