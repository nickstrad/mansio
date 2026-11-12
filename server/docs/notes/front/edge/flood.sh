#!/bin/bash
# Valid traffic through the TLS edge with and without a concurrent forged-ticket flood from the same source IP.
set -u
cd "$(dirname "$0")"
waitaddr() { for i in $(seq 300); do [ -s "$1" ] && [ "$(wc -l < "$1")" -ge 2 ] && return 0; sleep 0.02; done; exit 1; }
rm -f *.addr
cat > agentd.json <<J
{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","vm":"vm-local","db_dir":"dbs","keys_file":"keys/keys.json","log_file":"agentd-flood.log","tls":{"cert_file":"tls/agent.crt","key_file":"tls/agent.key"}}
J
bin/agentd -config agentd.json -addr-file agentd.addr 2>/dev/null & AP=$!
waitaddr agentd.addr; A=$(head -1 agentd.addr)
cat > edge.json <<J
{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","keys_file":"keys/keys.json","agents":{"vm-local":"https://$A"},"agent_tls":{"ca_file":"tls/ca.pem"},"tls":{"cert_file":"tls/edge.crt","key_file":"tls/edge.key"},"log_file":"edge-flood.log"}
J
bin/edge -config edge.json -addr-file edge.addr 2>/dev/null & EP=$!
waitaddr edge.addr; E=$(head -1 edge.addr); EH=$(sed -n 2p edge.addr)
FB="bin/frontbench run -mode http -dbs 16 -duration 8s -warmup 2s -url https://$E -proto h2 -ca tls/ca.pem"
echo "valid alone:      $($FB -key keys/issuer.key -conc 16)"
$FB -key keys-forged/issuer.key -conc 64 -duration 9s > forged.out &
FP=$!
echo "valid with flood: $($FB -key keys/issuer.key -conc 16)"
wait $FP; echo "forged flood:     $(cat forged.out)"
curl -s http://$EH/metrics | grep -E '^mansio_edge_tickets_total|^mansio_requests_total\{point="edge".*\} [1-9]'
kill -TERM $EP $AP; wait
