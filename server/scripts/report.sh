#!/usr/bin/env bash
# Usage: scripts/report.sh results/<run>   (default: the newest run)
# Runs queries/report.sql with DuckDB against one run's CSV files.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
run="${1:-$(ls -td "$root"/results/*/ 2>/dev/null | head -1)}"
[ -n "$run" ] && [ -f "$run/requests.csv" ] || { echo "no run found (expected results/<run>/requests.csv)" >&2; exit 1; }
command -v duckdb >/dev/null || { echo "duckdb CLI not found: brew install duckdb" >&2; exit 1; }
cd "$run"
echo "report: $(pwd)"
flags="-box"; duckdb -no-agent -c "SELECT 1" >/dev/null 2>&1 && flags="-no-agent -box"
duckdb $flags < "$root/queries/report.sql"
