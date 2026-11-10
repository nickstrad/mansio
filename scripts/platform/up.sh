#!/usr/bin/env bash
# Usage: scripts/platform/up.sh   (make platform-up)
#
# Brings the multi-node dev platform up and proves it works, unattended:
#   1. builds agentd, controld and the edge into .platform/bin
#   2. generates a throwaway dev CA and one server certificate (127.0.0.1/localhost), the controld/agent/edge
#      tokens and secrets files, and the ticket key ring (`controld keygen`)
#   3. starts Postgres and SeaweedFS (scripts/platform/compose.yml, Compose project `mansio-platform`)
#   4. starts controld (Postgres placement, tenancy in api-key mode), two durable agentd (vm-a, vm-b; S3
#      generations in bucket mansio-test) and the edge, every listener on HTTPS from the dev CA
#   5. runs scripts/platform/smoke.ts: registers the tic-tac-toe schema, creates a tenant and an API key, and
#      does one SDK write and one SDK read through the edge (locator route), checking the value read back.
#
# The steps are assembled from server/scripts/soak/run.sh (TLS, config shapes, process handling) and
# tests/e2e/platform/cluster.ts (tenancy, schema registry, SDK locator client).
#
# Everything it creates is in .platform/ (gitignored) and the `mansio-platform` Compose project;
# scripts/platform/down.sh removes exactly that. Daemons keep running after this script exits.
#
# Run again while the platform is up: nothing is rebuilt or restarted; it re-runs the smoke proof against the
# running platform (a new tenant each time). Leftover state from a platform that is no longer running: it
# refuses and asks for `make platform-down` first, rather than mixing old keys/secrets with new processes.
#
# Needs: go, docker (Compose v2), openssl, curl, jq, deno 2.9.5 (on PATH or in ~/.deno/bin).
# Ports (loopback): Postgres 15432, S3 18333 (PLATFORM_PG_PORT, PLATFORM_S3_PORT), controld 27080,
# vm-a 27070-27072, vm-b 27073-27075, edge 27443-27444.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=scripts/platform/lib.sh
source "$root/scripts/platform/lib.sh"

if [ -d "$state" ]; then
  if all_running; then
    log "platform already up ($state); re-running the smoke proof only"
    smoke
    exit 0
  fi
  die "$state exists but the platform is not (fully) running; run \`make platform-down\` first"
fi

for tool in go docker openssl curl jq; do command -v "$tool" >/dev/null || die "$tool is required"; done
command -v deno >/dev/null || die "deno is required (export PATH=\"\$HOME/.deno/bin:\$PATH\")"
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is required"
for p in "$pg_port" "$s3_port" 27080 27070 27071 27072 27073 27074 27075 27443 27444; do
  port_free "$p" || die "port $p is in use; is another platform running? (make platform-down)"
done

mkdir -p "$state"/{bin,tls,controld,edge,vm-a,vm-b}
chmod 700 "$state"
touch "$state/.mansio-platform" # down.sh deletes only a directory carrying this marker
# A failure from here on stops whatever this run started (processes, containers, state), so a failed
# platform-up leaves nothing behind.
# shellcheck disable=SC2154 # rc is assigned inside the trap string
trap 'rc=$?; if [ $rc -ne 0 ]; then log "platform-up failed (exit $rc); daemon logs follow, then teardown";
  tails; "$root/scripts/platform/down.sh" >&2 || true; fi' EXIT

# --- 1. Build. ---------------------------------------------------------------------------------------------
log "building agentd, controld, edge"
(cd "$root/server" && for c in agentd controld edge; do go build -o "$state/bin/$c" "./cmd/$c"; done)
bin="$state/bin"

# --- 2. Dev CA, certificate, ticket keys, secrets. ------------------------------------------------------------
log "generating the dev CA, server certificate, ticket keys and secrets"
tls="$state/tls"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj "/CN=mansio dev platform CA" \
  -keyout "$tls/ca.key" -out "$tls/ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=127.0.0.1" \
  -keyout "$tls/server.key" -out "$tls/server.csr" 2>/dev/null
printf 'subjectAltName=IP:127.0.0.1,DNS:localhost\nextendedKeyUsage=serverAuth\n' > "$tls/ext.cnf"
openssl x509 -req -in "$tls/server.csr" -CA "$tls/ca.pem" -CAkey "$tls/ca.key" -CAcreateserial \
  -days 30 -extfile "$tls/ext.cnf" -out "$tls/server.crt" 2>/dev/null
chmod 600 "$tls/"*.key

# The issuer's ring, seeded before controld starts (it signs with this key at once); the public half is what
# a static agentd/edge would take as keys_file. Here the agents and the edge fetch it from controld's /v1/keys.
"$bin/controld" keygen -dir "$state/controld/ticket-keys" -public "$state/ticket-keys.public.json" > "$state/kid"
log "ticket key $(cat "$state/kid")"

tok() { printf 'platform-%s-%s' "$1" "$(openssl rand -hex 16)"; }
control_token="$(tok control)" admin_token="$(tok admin)" edge_token="$(tok edge)"
pg_dsn="postgres://mansio:mansio@127.0.0.1:$pg_port/mansio?sslmode=disable"
umask 077
jq -n --arg pg "$pg_dsn" --arg c "$control_token" --arg a "$admin_token" --arg e "$edge_token" \
  --arg ak "$s3_access_key" --arg sk "$s3_secret_key" \
  '{pg_dsn:$pg, control_token:$c, admin_token:$a, edge_token:$e, s3_access_key:$ak, s3_secret_key:$sk}' \
  > "$state/controld/secrets.json"
printf '%s' "$edge_token" > "$state/edge/edge-token"
for vm in vm-a vm-b; do
  jq -n --arg pg "$pg_dsn" --arg c "$control_token" --arg ak "$s3_access_key" --arg sk "$s3_secret_key" \
    '{s3_access_key:$ak, s3_secret_key:$sk, control_token:$c, pg_dsn:$pg}' > "$state/$vm/secrets.json"
done
umask 022

# --- 3. Postgres and the object store. ----------------------------------------------------------------------
log "starting Postgres (:$pg_port) and SeaweedFS (:$s3_port) (compose project $project)"
compose up -d --wait --wait-timeout 120 >&2
compose exec -T postgres psql -q -U mansio -d mansio -c "create schema if not exists $pg_schema" >/dev/null
# The bucket is not created here: each agentd creates a missing bucket itself when it starts
# (objstore.EnsureBucket), and the two starting together both succeed.

# --- 4. Configs and daemons. ----------------------------------------------------------------------------------
s3="{\"endpoint\":\"127.0.0.1:$s3_port\",\"bucket\":\"$s3_bucket\",\"prefix\":\"platform/\"}"
api="https://127.0.0.1:27080"
jq -n --argjson s3 "$s3" --arg api "$api" '{
  listen: "127.0.0.1:27080",
  tls: {cert_file: "../tls/server.crt", key_file: "../tls/server.key"},
  secrets_file: "secrets.json",
  pg: {schema: "'"$pg_schema"'"},
  s3: $s3,
  agents: [
    {vm: "vm-a", endpoint: "127.0.0.1:27070", lifecycle: "http://127.0.0.1:27071"},
    {vm: "vm-b", endpoint: "127.0.0.1:27073", lifecycle: "http://127.0.0.1:27074"}
  ],
  ticket_keys: {dir: "ticket-keys"},
  tenancy: {mode: "api-key"},
  locator: {ticket_ttl: "60s"},
  log_file: "controld.log"
}' > "$state/controld/controld.json"

agent_config() { # VM QPORT HPORT APORT
  jq -n --argjson s3 "$s3" --arg api "$api" --arg vm "$1" --arg q "$2" --arg h "$3" --arg a "$4" '{
    listen: ("127.0.0.1:" + $q), health_listen: ("127.0.0.1:" + $h), admin_listen: ("127.0.0.1:" + $a),
    tls: {cert_file: "../tls/server.crt", key_file: "../tls/server.key"},
    vm: $vm,
    keys_url: ($api + "/v1/keys"), keys_file: "ticket-keys.json", control_ca_file: "../tls/ca.pem",
    schemas_url: $api,
    s3: $s3,
    secrets_file: "secrets.json",
    cache_dir: "cache", owner_dir: "owner",
    placement_pg: {schema: "'"$pg_schema"'"},
    maintenance: {interval: "1s", write_back_every: "10s"},
    log_file: "agentd.log"
  }' > "$state/$1/agentd.json"
}
agent_config vm-a 27070 27071 27072
agent_config vm-b 27073 27074 27075

jq -n --arg api "$api" '{
  listen: "127.0.0.1:27443", health_listen: "127.0.0.1:27444",
  tls: {cert_file: "../tls/server.crt", key_file: "../tls/server.key"},
  keys_url: ($api + "/v1/keys"), keys_file: "edge-ticket-keys.json", control_ca_file: "../tls/ca.pem",
  registry: {url: ($api + "/v1/agents"), token_file: "edge-token", refresh: "2s", timeout: "1s", agent_scheme: "https"},
  agent_tls: {ca_file: "../tls/ca.pem"},
  log_file: "edge.log"
}' > "$state/edge/edge.json"

# -dev: SeaweedFS is plain HTTP on loopback; every mansio listener here still speaks HTTPS.
log "starting controld"
start controld "$bin/controld" -dev -config "$state/controld/controld.json"
wait_ready controld "$api/readyz" --cacert "$tls/ca.pem"
for vm in vm-a vm-b; do
  log "starting agentd $vm"
  start "$vm" "$bin/agentd" -dev -config "$state/$vm/agentd.json"
done
wait_ready vm-a http://127.0.0.1:27071/readyz
wait_ready vm-b http://127.0.0.1:27074/readyz
log "starting the edge"
start edge "$bin/edge" -dev -config "$state/edge/edge.json"
wait_ready edge http://127.0.0.1:27444/readyz

jq -n --arg api "$api" --arg admin "$admin_token" \
  '{api: $api, edge: "https://127.0.0.1:27443", admin_token: $admin}' > "$state/smoke.json"
chmod 600 "$state/smoke.json"

# --- 5. Proof. --------------------------------------------------------------------------------------------------
smoke
trap - EXIT
log "platform up: controld $api, edge https://127.0.0.1:27443, CA $tls/ca.pem; \`make platform-down\` removes it"
