#!/usr/bin/env bash
# Free disk space used by coldload. Safe to run any time no coldload benchmark is running.
#
#   scripts/coldload/cleanup.sh [local]   cache dirs, *.part files, templates; caps coldload.log files at 5 MB
#   scripts/coldload/cleanup.sh objects   local + delete coldload's objects in SeaweedFS (all configs)
#   scripts/coldload/cleanup.sh all       objects + stop SeaweedFS and delete its Docker volume
#   scripts/coldload/cleanup.sh fuzz      Go's fuzz cache (go clean -fuzzcache; committed testdata/fuzz stays)
#
# Kept on purpose: results/coldload/<run>/*.csv and run.json (small; the evidence for docs/notes/ notes).
# Touches only coldload's files, bucket and container; never other Docker volumes or other agents' files.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
level="${1:-local}"
case "$level" in local|objects|all|fuzz) ;; *) sed -n '2,10p' "$0" >&2; exit 2 ;; esac

if [ -d results/.bench-lock ] && grep -q '^coldload' results/.bench-lock/owner 2>/dev/null; then
  pid="$(sed -n 's/.*pid \([0-9][0-9]*\).*/\1/p' results/.bench-lock/owner)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    echo "cleanup: a coldload benchmark is running ($(cat results/.bench-lock/owner)); try again later" >&2
    exit 1
  fi
fi

kib() { du -sk "$@" 2>/dev/null | awk '{s += $1} END {print s + 0}'; }
volume_mb() { docker system df -v 2>/dev/null | awk '$1 == "seaweedfs_seaweed-data" {print $3}'; }

if [ "$level" = fuzz ]; then
  before=$(kib "$(go env GOCACHE)/fuzz")
  go clean -fuzzcache
  echo "fuzz: freed $(( before / 1024 )) MiB of Go fuzz cache"
  exit 0
fi

before=$(kib results/coldload)
rm -rf results/coldload/*/cache results/coldload/templates
find results/coldload -name '*.part' -delete 2>/dev/null || true
for log in results/coldload/*/coldload.log; do   # cap logs: keep the newest ~5 MB
  [ -f "$log" ] && [ "$(wc -c < "$log")" -gt 5242880 ] && { tail -c 5242880 "$log" > "$log.tmp" && mv "$log.tmp" "$log"; }
done
echo "local: freed $(( (before - $(kib results/coldload)) / 1024 )) MiB under results/coldload (CSV results kept)"

if [ "$level" = objects ] || [ "$level" = all ]; then
  if curl -s -o /dev/null localhost:8333; then
    echo "objects: SeaweedFS volume before: $(volume_mb)"
    go build -o bin/coldload ./cmd/bench/coldload
    for cfg in configs/coldload/*.json; do bin/coldload -config "$cfg" clean 2>&1 | grep -E "clean:|rror" || true; done
  else
    echo "objects: SeaweedFS not running; skipped"
  fi
fi

if [ "$level" = all ]; then
  docker compose -f deploy/seaweedfs/docker-compose.yml down -v 2>&1 | grep -E "Removed|Stopped" || true
  echo "all: SeaweedFS stopped and its volume removed. Docker Desktop's Docker.raw shrinks on its own over time."
fi
echo "host free: $(df -h "$root" | awk 'NR == 2 {print $4}')"
