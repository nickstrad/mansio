#!/bin/sh
# Run the e2e suite (`deno task e2e`) for this checkout inside Linux, against BACKEND=deno or BACKEND=go.
#
# Why: the suite needs Linux (/proc process groups, setsid, the SQLite FFI binding refuses darwin/aarch64) and a
# Postgres. This starts postgres:17 as a sibling container on a private network (E2E_POSTGRES_URL, so main.ts
# starts no Compose project), cross-compiles agentd for Linux on the host (AGENTD_BIN; the container has no Go),
# and runs the suite in denoland/deno:2.9.5 with the checkout and its git directory mounted at their host paths
# (main.ts checks `git status` before and after). For go it also cross-compiles dbtool (DBTOOL_BIN) and starts
# a SeaweedFS sibling (E2E_S3_*) for the durable agents of backup_restore (tests/e2e/platform/durable.ts), and
# controld and the edge (CONTROLD_BIN, EDGE_BIN) for scale_out's cluster (tests/e2e/platform/cluster.ts).
#
# usage: work-scripts/bash/e2e-linux.sh deno|go [deno test args, e.g. --filter documents]
set -eu
backend=${1:-deno}
[ $# -gt 0 ] && shift
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

# S3 for the durable agents (go only): SeaweedFS with one dev identity (public test values).
s3env=""
if [ "$backend" = go ]; then
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
fi

# shellcheck disable=SC2086 # $s3env is a list of flags
docker run --rm --network "$net" -v "$root":"$root" -v "$common":"$common" \
  -v "mansio-nm-$tag":"$root/node_modules" -v mansio-deno-dir:/deno-dir -v "$bin":/e2e-bin \
  -e DENO_DIR=/deno-dir -e BACKEND="$backend" -e AGENTD_BIN=/e2e-bin/agentd -e DBTOOL_BIN=/e2e-bin/dbtool \
  -e CONTROLD_BIN=/e2e-bin/controld -e EDGE_BIN=/e2e-bin/edge $s3env \
  -e E2E_POSTGRES_URL=postgres://mansio:mansio@pg:5432/postgres -w "$root" denoland/deno:2.9.5 \
  sh -c 'apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq libsqlite3-0 git procps >/dev/null 2>&1;
    git config --global --add safe.directory "*"; deno task e2e "$@"' sh "$@"
