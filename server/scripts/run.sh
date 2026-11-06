#!/usr/bin/env bash
# Usage: scripts/run.sh [config]   Build, run one benchmark, then print its DuckDB report.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
config="${1:-configs/smoke.json}"
name="$(basename "$config" .json)"
out="results/${name}-$(date +%Y%m%d-%H%M%S)"
go build -o bin/sqlbench ./cmd/sqlbench
LOCK_WHO=sqlbench scripts/lock.sh results/.bench-lock bin/sqlbench -config "$config" -out "$out"
scripts/report.sh "$out"
