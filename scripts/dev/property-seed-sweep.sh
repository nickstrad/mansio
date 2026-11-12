#!/usr/bin/env bash
# property-seed-sweep.sh — run one property test file (optionally one test, by name filter) over a seed range.
# Prints one line per seed: seed, PASS|FAIL|CRASH(139), the first assertion error (or -), and any coverage line (or -).
# A summary of outcomes and error messages follows the per-seed lines.
#
# Usage:  bash scripts/dev/property-seed-sweep.sh <test-file> <name-filter|-> [first=1] [last=200] [parallel=3]
# Example: bash scripts/dev/property-seed-sweep.sh tests/property/offsets_test.ts "exactly-once" 1 200
# Env:    KEEP_RAW=<dir> keeps each seed's deno output as <dir>/<seed>.txt (otherwise a temp dir is used).
# Needs:  deno on PATH. Runs from the repo root. Parallelism is capped at 3 (8 GB VM).
#
# A starting point for gating on a seed sweep. A seed is deterministic, so a failing seed fails every run — a
# rerun will not go green. Only exit 139 is truly run-to-run intermittent.
set -euo pipefail
cd "$(dirname "$0")/../.."
export PATH="$HOME/.deno/bin:$PATH" NO_COLOR=1

file="${1:?usage: property-seed-sweep.sh <test-file> <name-filter|-> [first] [last] [parallel]}"
filter="${2:?name filter required (use - for the whole file)}"
first="${3:-1}"
last="${4:-200}"
par="${5:-3}"
((par > 3)) && par=3
raw="${KEEP_RAW:-$(mktemp -d)}"
mkdir -p "$raw"
export file filter raw

run_one() {
  local seed="$1" rc=0
  local args=(test -A "$file")
  if [[ "$filter" != "-" ]]; then args+=(--filter "$filter"); fi
  PROP_SEED="$seed" deno "${args[@]}" >"$raw/$seed.txt" 2>&1 || rc=$?
  echo "exit=$rc" >>"$raw/$seed.txt"
}
export -f run_one

seq "$first" "$last" | xargs -P "$par" -I{} bash -c 'run_one "$1"' _ {}

for seed in $(seq "$first" "$last"); do
  f="$raw/$seed.txt"
  rc="$(sed -n 's/^exit=//p' "$f")"
  case "$rc" in
    0) status=PASS ;;
    139) status=CRASH\(139\) ;;
    *) status=FAIL ;;
  esac
  err="$(grep -m1 -E '^error: ' "$f" | sed 's/^error: //' || true)"
  cov="$(grep -m1 -o 'coverage: .*' "$f" || true)"
  printf '%s\t%s\t%s\t%s\n' "$seed" "$status" "${err:--}" "${cov:--}"
done

echo "--- summary (raw output in $raw)"
for seed in $(seq "$first" "$last"); do
  f="$raw/$seed.txt"
  rc="$(sed -n 's/^exit=//p' "$f")"
  case "$rc" in 0) echo PASS ;; 139) echo CRASH ;; *) echo FAIL ;; esac
done | sort | uniq -c
