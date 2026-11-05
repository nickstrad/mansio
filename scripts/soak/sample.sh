#!/usr/bin/env bash
# The soak's sampler (scripts/soak/run.sh starts it): every SAMPLE_EVERY s, one row per series into
# $OUT/samples.tsv (t, proc, series, value): each daemon's /metrics (minus histogram buckets), its RSS from ps,
# the agents' cache-dir bytes, and the bucket's object count and bytes under PREFIX (aws CLI, every other
# sample). A daemon that does not answer (restarting, stopped) is skipped for that sample.
# Env: OUT, CA, PREFIX, SAMPLE_EVERY; AWS_* for the bucket listing.
set -uo pipefail
f="$OUT/samples.tsv"
[ -s "$f" ] || printf 't\tproc\tseries\tvalue\n' > "$f"
procs=(controld vm-a vm-b edge)
urls=(https://127.0.0.1:17080/metrics http://127.0.0.1:17071/metrics http://127.0.0.1:17074/metrics http://127.0.0.1:17444/metrics)
n=0
while :; do
  t="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  {
    for i in "${!procs[@]}"; do
      p="${procs[$i]}"
      curl -sf -m 5 --cacert "$CA" "${urls[$i]}" | awk -v t="$t" -v p="$p" \
        '!/^#/ && !/_bucket\{/ && NF >= 2 { v = $NF; $NF = ""; sub(/ +$/, ""); print t "\t" p "\t" $0 "\t" v }'
      if pid="$(cat "$OUT/$p.pid" 2>/dev/null)" && rss="$(ps -o rss= -p "$pid" 2>/dev/null)"; then
        printf '%s\t%s\tps_rss_bytes\t%s\n' "$t" "$p" "$(( ${rss// /} * 1024 ))"
      fi
    done
    for vm in vm-a vm-b; do
      for d in cache owner; do
        [ -d "$OUT/$vm/$d" ] && printf '%s\t%s\tdir_bytes{dir="%s"}\t%s\n' "$t" "$vm" "$d" "$(( $(du -sk "$OUT/$vm/$d" 2>/dev/null | cut -f1) * 1024 ))"
      done
    done
    if (( n % 2 == 0 )); then
      aws --endpoint-url http://localhost:8333 s3 ls --recursive --summarize "s3://coldload/$PREFIX" 2>/dev/null |
        awk -v t="$t" '/Total Objects:/ { print t "\tbucket\tobjects\t" $3 } /Total Size:/ { print t "\tbucket\tbytes\t" $3 }'
    fi
  } >> "$f"
  n=$((n + 1))
  sleep "$SAMPLE_EVERY"
done
