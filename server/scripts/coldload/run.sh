#!/usr/bin/env bash
# Usage: scripts/coldload/run.sh configs/coldload/<name>.json [seed|run|all]
# Starts SeaweedFS if needed, builds, runs, prints the report.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
cfg="${1:?config}"; cmd="${2:-all}"
if ! curl -s -o /dev/null localhost:8333; then
  docker compose -f deploy/seaweedfs/docker-compose.yml up -d
  for _ in $(seq 30); do curl -s -o /dev/null localhost:8333 && break; sleep 1; done
fi
go build -o bin/coldload ./cmd/bench/coldload

# Disk guard before seeding: at least COLDLOAD_MIN_FREE_GB (20) free on the host and COLDLOAD_MIN_VM_FREE_GB
# (12; the sizes config stores ~9.3 GB) in the Docker VM that holds SeaweedFS's volume.
host_min="${COLDLOAD_MIN_FREE_GB:-20}"; vm_min="${COLDLOAD_MIN_VM_FREE_GB:-12}"
host_gb=$(df -k "$root" | awk 'NR == 2 {print int($4 / 1048576)}')
vm_gb=$(docker exec coldload-seaweedfs df -k /data 2>/dev/null | awk 'NR == 2 {print int($4 / 1048576)}')
vm_gb="${vm_gb:-$vm_min}"
if [ "$cmd" != run ] && { [ "$host_gb" -lt "$host_min" ] || [ "$vm_gb" -lt "$vm_min" ]; }; then
  echo "run.sh: ${host_gb} GB free on the host (need ${host_min}), ${vm_gb} GB in the Docker VM (need ${vm_min})." >&2
  echo "        Free space first: scripts/coldload/cleanup.sh all" >&2
  exit 1
fi

# Shared benchmark lock (see ../agent-work/server/state_files/STATE-coldload.md "Coordination"): one benchmark at a time on this machine.
lock="$root/results/.bench-lock"
if [ "$cmd" = run ] || [ "$cmd" = all ]; then
  mkdir -p "$root/results"
  until mkdir "$lock" 2>/dev/null; do
    echo "waiting for benchmark lock held by: $(cat "$lock/owner" 2>/dev/null || echo unknown)" >&2
    sleep 10
  done
  printf 'coldload %s %s pid %s started %s\n' "$cfg" "$cmd" "$$" "$(date '+%F %T')" > "$lock/owner"
fi
# Always tidy local files (cache, *.part, templates), even after a failure; then release the lock.
trap 'if grep -q "pid $$ " "$lock/owner" 2>/dev/null; then rm -rf "$lock"; fi; scripts/coldload/cleanup.sh local >/dev/null 2>&1 || true' EXIT
bin/coldload -config "$cfg" "$cmd"
if [ "$cmd" != seed ]; then
  name="$(sed -n 's/.*"name": *"\([^"]*\)".*/\1/p' "$cfg" | head -1)"  # first "name" in the file is the config's
  scripts/coldload/report.sh "results/coldload/$name"
fi
