#!/usr/bin/env bash
# Usage: scripts/sim-swarm.sh [seeds] [first]        (env: SIM_SEEDS, SIM_FIRST, SIM_SHRINK=0 to skip shrinking)
#
# DST-3, the nightly swarm (IR protocol plan §6.4 item 4): runs <seeds> cluster simulations (default 2000) with
# fault rates varied per seed (MANSIO_SIM_SWARM=1), and <seeds>/4 single-node (DST-1) seeds, from <first>
# (default: a new range each day). Each failing cluster seed is shrunk (sim.ShrinkCluster) and written as a
# regression vector, internal/sim/testdata/cluster/seed-<s>.json: fix the bug, edit the vector's "why",
# commit both. Output, replay commands and vectors under results/sim-swarm/<time>/. Exits 1 on any failure.
# No CI runs it; run it by hand or from cron. About 0.5–5 s per cluster seed per core.
set -euo pipefail
cd "$(dirname "$0")/.."
root="$PWD"
seeds="${1:-${SIM_SEEDS:-2000}}"
first="${2:-${SIM_FIRST:-$(( $(date +%s) / 86400 % 100000 * 10000 + 1 ))}}"
out="$root/results/sim-swarm/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$out"
echo "sim-swarm: $seeds cluster seeds from $first; output in $out"
go test -c -o "$out/sim.test" ./internal/sim
cd internal/sim
status=0
MANSIO_SIM_SWARM=1 MANSIO_SIM_SEEDS="$seeds" MANSIO_SIM_FIRST="$first" \
  "$out/sim.test" -test.run '^TestClusterSeeds$' -test.v -test.timeout 24h > "$out/cluster.txt" 2>&1 || status=1
MANSIO_SIM_SEEDS="$(( seeds / 4 ))" MANSIO_SIM_FIRST="$first" \
  "$out/sim.test" -test.run '^TestSeeds$' -test.timeout 24h > "$out/single.txt" 2>&1 || status=1
failing="$(grep -oE -- '--- FAIL: TestClusterSeeds/[0-9]+' "$out/cluster.txt" | sed 's|.*/||' | sort -un || true)"
single="$(grep -oE -- '--- FAIL: TestSeeds/[0-9]+' "$out/single.txt" | sed 's|.*/||' | sort -un || true)"
for s in $failing; do
  echo "cluster seed $s failed; replay: MANSIO_SIM_SWARM=1 MANSIO_SIM_SEED=$s MANSIO_SIM_TRACE=1 go test ./internal/sim -run TestClusterSeeds -v"
  if [ "${SIM_SHRINK:-1}" != 0 ]; then
    MANSIO_SIM_SWARM=1 MANSIO_SIM_SHRINK="$s" MANSIO_SIM_SAVE=1 "$out/sim.test" -test.run '^TestClusterShrink$' -test.v \
      -test.timeout 2h > "$out/shrink-$s.txt" 2>&1 || true
    grep -E "shrunk in|^\s+cluster_test.go:[0-9]+:   (op|t=)" "$out/shrink-$s.txt" || true
  fi
done
for s in $single; do
  echo "single-node seed $s failed; replay: MANSIO_SIM_SEED=$s MANSIO_SIM_TRACE=1 go test ./internal/sim -run TestSeeds -v"
done
echo "sim-swarm: cluster $(grep -c -- '--- PASS: TestClusterSeeds/' "$out/cluster.txt" || true) passed, $(echo "$failing" | grep -c . || true) failed;" \
  "single-node failed: $(echo "$single" | grep -c . || true)"
exit "$status"
