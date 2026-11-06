#!/usr/bin/env bash
# Usage: scripts/soak/analyze.sh RUN-DIR   — runs analyze.sql (DuckDB) over a soak run's samples, marks and events.
set -euo pipefail
dir="$(cd "$1" && pwd)"
{ echo "set variable dir = '$dir';"; cat "$(dirname "$0")/analyze.sql"; } | duckdb
