#!/usr/bin/env bash
# Usage: scripts/coldload/report.sh results/coldload/<run>   (default: newest run)
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
run="${1:-$(dirname "$(ls -t "$root"/results/coldload/*/activations.csv 2>/dev/null | head -1)")}"
[ -f "$run/activations.csv" ] || { echo "no run found at $run" >&2; exit 1; }
command -v duckdb >/dev/null || { echo "duckdb CLI not found: brew install duckdb" >&2; exit 1; }
cd "$run"
echo "report: $(pwd)"
flags="-box"; duckdb -no-agent -c "SELECT 1" >/dev/null 2>&1 && flags="-no-agent -box"
duckdb $flags < "$root/queries/coldload/report.sql"
