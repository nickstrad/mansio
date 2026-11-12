#!/usr/bin/env bash
# Usage: scripts/front/bench.sh [out-dir]   (run it under the bench lock: make front-bench)
#
# The front's measurement matrix: the same point reads and inserts sent in-process to the actor, through the
# in-process pipeline, and through agentd over HTTP/1.1 and h2c; plus the micro benchmarks that split the
# per-request overhead (ticket verify, cache hit, decode, encode, admission, loopback HTTP).
# Env: DBS (16), CONC (64), DURATION (10s).
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
out="${1:-$root/results/front/$(date -u +%Y-%m-%dT%H%M%SZ)}"
dbs="${DBS:-16}" conc="${CONC:-64}" dur="${DURATION:-10s}"
mkdir -p "$out/bin"
cd "$root"
go build -o "$out/bin/agentd" ./cmd/agentd
go build -o "$out/bin/frontbench" ./cmd/bench/frontbench
fb="$out/bin/frontbench"
"$fb" keygen -out "$out/keys"
"$fb" seed -dir "$out/dbs" -dbs "$dbs"
cat > "$out/agentd.json" <<JSON
{"listen":"127.0.0.1:0","vm":"vm-local","db_dir":"dbs","keys_file":"keys/keys.json","log_file":"agentd.log",
 "statement_timeout":"2s","admission":{"tenant_in_flight":4096,"tenant_queue":4096,"db_in_flight":1024,"db_queue":1024}}
JSON
"$out/bin/agentd" -config "$out/agentd.json" -addr-file "$out/addr" 2> "$out/agentd.stderr" &
agent=$!
trap 'kill $agent 2>/dev/null || true' EXIT
for _ in $(seq 100); do [ -s "$out/addr" ] && break; sleep 0.1; done
addr="$(cat "$out/addr")"
common=(-dbs "$dbs" -conc "$conc" -duration "$dur" -warmup 2s)
for w in 0 1; do
  name=$([ "$w" = 0 ] && echo read || echo write)
  "$fb" run -mode inproc -dir "$out/dbs" -write "$w" -out "$out/inproc-$name.json" "${common[@]}"
  "$fb" run -mode pipeline -dir "$out/dbs" -write "$w" -out "$out/pipeline-$name.json" "${common[@]}"
  for p in h1 h2c; do
    "$fb" run -mode http -proto "$p" -url "http://$addr" -key "$out/keys/issuer.key" -write "$w" \
      -out "$out/http-$p-$name.json" "${common[@]}"
  done
done | tee "$out/matrix.txt"
kill -TERM $agent; wait $agent || true
go test -run '^$' -bench 'Verify|Mint|CacheHit' -benchtime 2s ./internal/ticket | tee "$out/micro.txt"
go test -run '^$' -bench . -benchtime 2s ./internal/front | tee -a "$out/micro.txt"
echo "results in $out"
