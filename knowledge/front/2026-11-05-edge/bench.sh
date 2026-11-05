#!/bin/bash
# CP-7 edge overhead: frontbench direct to agentd vs through the edge, plain (-dev) and TLS (private CA).
set -u
cd "$(dirname "$0")"
FB="bin/frontbench run -mode http -key keys/issuer.key -dbs 16 -conc ${CONC:-64} -duration ${DUR:-8s} -warmup 2s"
waitaddr() { for i in $(seq 300); do [ -s "$1" ] && [ "$(wc -l < "$1")" -ge 2 ] && return 0; sleep 0.02; done; echo "no $1"; exit 1; }
for phase in plain tls; do
  rm -f *.addr
  if [ $phase = plain ]; then
    cat > agentd.json <<J
{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","vm":"vm-local","db_dir":"dbs","keys_file":"keys/keys.json","log_file":"agentd-$phase.log"}
J
  else
    cat > agentd.json <<J
{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","vm":"vm-local","db_dir":"dbs","keys_file":"keys/keys.json","log_file":"agentd-$phase.log","tls":{"cert_file":"tls/agent.crt","key_file":"tls/agent.key"}}
J
  fi
  bin/agentd -config agentd.json -addr-file agentd.addr 2>agentd-$phase.err & AP=$!
  waitaddr agentd.addr; A=$(head -1 agentd.addr)
  if [ $phase = plain ]; then
    cat > edge.json <<J
{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","keys_file":"keys/keys.json","agents":{"vm-local":"http://$A"},"log_file":"edge-$phase.log"}
J
    bin/edge -config edge.json -addr-file edge.addr -dev 2>edge-$phase.err & EP=$!
  else
    cat > edge.json <<J
{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","keys_file":"keys/keys.json","agents":{"vm-local":"https://$A"},"agent_tls":{"ca_file":"tls/ca.pem"},"tls":{"cert_file":"tls/edge.crt","key_file":"tls/edge.key"},"log_file":"edge-$phase.log"}
J
    bin/edge -config edge.json -addr-file edge.addr 2>edge-$phase.err & EP=$!
  fi
  waitaddr edge.addr; E=$(head -1 edge.addr); EH=$(sed -n 2p edge.addr)
  sleep 0.5
  for w in 0 0.2; do
    if [ $phase = plain ]; then
      echo "direct  plain h1   w=$w: $($FB -write $w -url http://$A -proto h1 -out direct-$phase-$w.json)"
      echo "edge    plain h1   w=$w: $($FB -write $w -url http://$E -proto h1 -out edge-$phase-$w.json)"
    else
      echo "direct  tls   h2   w=$w: $($FB -write $w -url https://$A -proto h2 -ca tls/ca.pem -out direct-$phase-$w.json)"
      echo "edge    tls   h2   w=$w: $($FB -write $w -url https://$E -proto h2 -ca tls/ca.pem -out edge-$phase-$w.json)"
    fi
  done
  curl -s http://$EH/metrics | grep -E '^tsdb_edge_tickets_total|^tsdb_edge_agent_answers_total|^tsdb_requests_total\{point="edge".*\} [1-9]|^tsdb_breaker_state.*open.* 1' > edge-$phase.metrics
  kill -TERM $EP; wait $EP; echo "edge exit $?"
  kill -TERM $AP; wait $AP; echo "agentd exit $?"
done
