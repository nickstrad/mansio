#!/usr/bin/env bash
# Usage: scripts/compare.sh   Compares every run under results/ with queries/compare.sql.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
flags="-box"; duckdb -no-agent -c "SELECT 1" >/dev/null 2>&1 && flags="-no-agent -box"
duckdb $flags < queries/compare.sql
