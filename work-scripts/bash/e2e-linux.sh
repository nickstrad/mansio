#!/bin/sh
# Run the e2e suite (`deno task e2e`) for this checkout inside Linux, against BACKEND=deno or BACKEND=go.
#
# Why: the suite needs Linux (/proc process groups, setsid, the SQLite FFI binding refuses darwin/aarch64) and a
# Postgres. This starts postgres:17 as a sibling container on a private network (E2E_POSTGRES_URL, so main.ts
# starts no Compose project), cross-compiles agentd for Linux on the host (AGENTD_BIN; the container has no Go),
# and runs the suite in denoland/deno:2.9.5 with the checkout and its git directory mounted at their host paths
# (main.ts checks `git status` before and after).
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
bin=$(mktemp -d)
cleanup() {
  docker rm -f "$pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$bin"
}
trap cleanup EXIT INT TERM

case $(uname -m) in arm64 | aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
(cd "$root/server" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$bin/agentd" ./cmd/agentd)

docker network create "$net" >/dev/null
docker run -d --name "$pg" --network "$net" --network-alias pg --tmpfs /var/lib/postgresql/data \
  -e POSTGRES_USER=mansio -e POSTGRES_PASSWORD=mansio -e POSTGRES_DB=postgres postgres:17 >/dev/null
i=0
until docker exec "$pg" pg_isready -U mansio -d postgres >/dev/null 2>&1; do
  i=$((i + 1))
  [ $i -lt 120 ] || { echo "postgres did not start" >&2; exit 1; }
  sleep 0.5
done

docker run --rm --network "$net" -v "$root":"$root" -v "$common":"$common" \
  -v "mansio-nm-$tag":"$root/node_modules" -v mansio-deno-dir:/deno-dir -v "$bin":/e2e-bin \
  -e DENO_DIR=/deno-dir -e BACKEND="$backend" -e AGENTD_BIN=/e2e-bin/agentd \
  -e E2E_POSTGRES_URL=postgres://mansio:mansio@pg:5432/postgres -w "$root" denoland/deno:2.9.5 \
  sh -c 'apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq libsqlite3-0 git procps >/dev/null 2>&1;
    git config --global --add safe.directory "*"; deno task e2e "$@"' sh "$@"
