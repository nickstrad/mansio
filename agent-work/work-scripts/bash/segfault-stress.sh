#!/usr/bin/env bash
# segfault-stress.sh — stress runner for the intermittent exit-139 (SIGSEGV) seen in mansio test runs.
#
# Runs one command N times, J at a time, and records every exit code plus the child pid. Any run that
# dies on a signal (exit >= 128) gets the matching `dmesg` lines captured: the kernel logs the faulting
# thread id in brackets (`tokio-runtime-w[3487262]`), so the script greps `dmesg` for thread ids in
# [child pid, child pid + 2000] instead of attributing by timing. This distinguishes an OOM kill
# (SIGKILL, 137, with an "Out of memory" line) from a native SIGSEGV (139, with a "segfault at ..." line).
#
# Known so far (sq-07, corrected by the Opus 5.5 review): the faulting thread is a Deno tokio
# blocking-pool thread created at startup; the code it was running when it jumped to a bad address is
# unknown and may be DuckDB/N-API work dispatched onto that pool — "not DuckDB" does NOT follow.
# Crashes occur at -j 1 (parallelism is not required for the crash). Output lands in an outdir, plus a
# summary on stdout.
#
# Usage:
#   bash work-scripts/bash/segfault-stress.sh [-n RUNS] [-j JOBS] [-s SEED_START] [-o OUTDIR] -- CMD [ARGS...]
#
#   -n RUNS        how many runs (default 20)
#   -j JOBS        concurrent runs, capped at 3 (default 1); the 8 GB VM OOM-killed at higher parallelism
#   -s SEED_START  if set, run i gets PROP_SEED=SEED_START+i (replays a property seed sweep)
#   -o OUTDIR      where logs go (default: a new dir under /tmp, printed at the start)
#
# Environment: the command inherits the caller's environment. A PROP_SEED set in the caller's environment is
# recorded in results.tsv (unless -s overrides it).
#
# Example:
#   bash work-scripts/bash/segfault-stress.sh -n 20 -j 1 -- deno test -A tests/property/offsets_test.ts
#
# Outputs per run i: OUTDIR/run-<i>.log (stdout+stderr), and OUTDIR/run-<i>.dmesg (only when exit >= 128:
# the dmesg lines whose thread id is in [child pid, child pid+2000], or a note that none matched).
# OUTDIR/results.tsv: run, exit, seconds, PROP_SEED, child pid. Exit status of this script: 0 always (it
# reports, it does not judge); read the summary.

set -euo pipefail

runs=20
jobs=1
seed_start=""
outdir=""

while getopts ":n:j:s:o:" opt; do
  case "$opt" in
    n) runs="$OPTARG" ;;
    j) jobs="$OPTARG" ;;
    s) seed_start="$OPTARG" ;;
    o) outdir="$OPTARG" ;;
    *) echo "usage: $0 [-n RUNS] [-j JOBS] [-s SEED_START] [-o OUTDIR] -- CMD [ARGS...]" >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
[ "${1:-}" = "--" ] && shift
if [ "$#" -eq 0 ]; then
  echo "error: no command given after --" >&2
  exit 2
fi
if [ "$jobs" -gt 3 ]; then
  echo "error: -j capped at 3 on this 8 GB VM (got $jobs)" >&2
  exit 2
fi

outdir="${outdir:-$(mktemp -d /tmp/segfault-stress.XXXXXX)}"
mkdir -p "$outdir"
results="$outdir/results.tsv"
printf 'run\texit\tseconds\tprop_seed\tpid\n' > "$results"
echo "outdir: $outdir"
echo "command: $*  (runs=$runs jobs=$jobs seed_start=${seed_start:-unset})"

run_one() {
  local i="$1"
  shift
  local seed_env=()
  if [ -n "$seed_start" ]; then
    seed_env=(env "PROP_SEED=$((seed_start + i))")
  fi
  local log="$outdir/run-$i.log"
  local start end code=0
  start=$(date +%s.%N)
  # Background + wait records the child pid for dmesg attribution; `|| code=$?` keeps set -e from
  # aborting the loop on a failing run.
  "${seed_env[@]}" "$@" > "$log" 2>&1 &
  local child_pid=$!
  wait "$child_pid" || code=$?
  end=$(date +%s.%N)
  local secs
  secs=$(awk -v a="$start" -v b="$end" 'BEGIN { printf "%.1f", b - a }')
  local seed_val="${seed_start:+$((seed_start + i))}"
  seed_val="${seed_val:-${PROP_SEED:-}}"
  if [ "$code" -ge 128 ]; then
    # Thread ids are bracketed in dmesg lines; match the ones near the child pid instead of
    # attributing by timing (a timing match once misattributed another process's crash to ours).
    dmesg 2>/dev/null | awk -v pid="$child_pid" '
      { if (match($0, /\[[0-9]+\]/)) {
          tid = substr($0, RSTART + 1, RLENGTH - 2) + 0;
          if (tid >= pid && tid <= pid + 2000) print;
      } }' > "$outdir/run-$i.dmesg" || true
    if [ ! -s "$outdir/run-$i.dmesg" ]; then
      printf 'no dmesg line matched a thread id in [%s, %s] (the kernel may not have logged this crash)\n' \
        "$child_pid" "$((child_pid + 2000))" > "$outdir/run-$i.dmesg"
    fi
  fi
  printf '%s\t%s\t%s\t%s\t%s\n' "$i" "$code" "$secs" "${seed_val:--}" "$child_pid" >> "$results.part.$i"
}

# Launch with at most $jobs in flight. Each run writes its own part file, merged in order at the end, so
# concurrent appends to results.tsv cannot interleave.
for ((i = 1; i <= runs; i++)); do
  while [ "$(jobs -rp | wc -l)" -ge "$jobs" ]; do
    sleep 0.2
  done
  run_one "$i" "$@" &
done
wait

for ((i = 1; i <= runs; i++)); do
  cat "$results.part.$i" >> "$results"
  rm -f "$results.part.$i"
done

awk -F'\t' 'NR > 1 {
  n++
  if ($2 == 0) pass++
  else if ($2 == 139) seg++
  else if ($2 == 137) killed++
  else if ($2 >= 128) sig++
  else other++
}
END {
  printf "runs=%d pass=%d segfault(139)=%d sigkill(137)=%d other_signal=%d other_nonzero=%d\n",
    n, pass + 0, seg + 0, killed + 0, sig + 0, other + 0
}' "$results" | tee "$outdir/summary.txt"
