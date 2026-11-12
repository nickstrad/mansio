#!/usr/bin/env bash
# Usage: scripts/soak/run.sh [out-dir]
#
# The soak: the whole system under load, faults and idle hours over time, checked for steady state. Starts,
# all over TLS from a throwaway private CA:
#   controld (Postgres :5433, a fresh schema) — locator, issuer, VM registry
#   two agentd (vm-a, vm-b) in S3 durable mode on SeaweedFS :8333 (the agent identity, a fresh prefix)
#   the edge (routes from controld's VM registry)
# then drives `frontbench locator -edge` (internal/client: resolves at controld, queries through the edge) over
# many databases with a skewed mix, in three phases: load with fault windows, near-idle, clean load. A sampler
# records every daemon's /metrics, RSS, cache-dir and bucket bytes every SAMPLE_EVERY s. At the end
# `frontbench verify` checks every acknowledged write against the rows, and analyze.sql (DuckDB) computes the
# slopes and the error attribution.
#
# Plan and fault times: configs/soak/soak.env (override any of them from the environment; NOFAULTS=1 skips
# the fault windows, a phase of 0 s is skipped, RATE=0 is closed loop: capacity runs). Needs: go, docker
# (mansio-postgres, coldload-seaweedfs running), openssl, curl, jq, aws (bucket listing), duckdb.
# Leaves the containers running (SeaweedFS is paused, then unpaused, in its fault window); stops every process it started.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
source "$root/configs/soak/soak.env"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
out="${1:-$root/results/soak/$stamp}"
mkdir -p "$out/bin" "$out/tls" "$out/vm-a" "$out/vm-b"
out="$(cd "$out" && pwd)"
id="$(echo "$stamp" | tr 'A-Z' 'a-z')"
schema="soak_$id" prefix="soak-$id/"
pg_dsn='postgres://mansio:mansio-dev@localhost:5433/mansio?sslmode=disable'
admin_token="soak-admin-$(openssl rand -hex 12)" control_token="soak-control-$(openssl rand -hex 12)"
edge_token="soak-edge-$(openssl rand -hex 12)"
export AWS_ACCESS_KEY_ID=coldload AWS_SECRET_ACCESS_KEY=coldload-secret AWS_DEFAULT_REGION=us-east-1
log() { echo "$(date -u +%H:%M:%S) $*" | tee -a "$out/run.log" >&2; }
now() { perl -MTime::HiRes=time -MPOSIX=strftime -e '$t = time; printf "%s.%03dZ", strftime("%Y-%m-%dT%H:%M:%S", gmtime($t)), ($t - int($t)) * 1000'; }
mark() { printf '%s\t%s\t%s\n' "$(now)" "$1" "$2" >> "$out/marks.tsv"; log "mark $1 $2"; }
printf 't\tkind\twhat\n' > "$out/marks.tsv"

cd "$root"
log "building into $out/bin"
for c in agentd controld edge frontbench mansioctl; do go build -o "$out/bin/$c" "./cmd/$c"; done
bin="$out/bin"

# --- TLS: one throwaway CA, one server certificate for 127.0.0.1/localhost (every daemon here). -----------
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7 -subj "/CN=mansio soak CA" \
  -keyout "$out/tls/ca.key" -out "$out/tls/ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=127.0.0.1" \
  -keyout "$out/tls/server.key" -out "$out/tls/server.csr" 2>/dev/null
printf 'subjectAltName=IP:127.0.0.1,DNS:localhost\nextendedKeyUsage=serverAuth\n' > "$out/tls/ext.cnf"
openssl x509 -req -in "$out/tls/server.csr" -CA "$out/tls/ca.pem" -CAkey "$out/tls/ca.key" -CAcreateserial \
  -days 7 -extfile "$out/tls/ext.cnf" -out "$out/tls/server.crt" 2>/dev/null
chmod 600 "$out/tls/"*.key
ca="$out/tls/ca.pem"

# --- Postgres schema, configs, secrets. -------------------------------------------------------------------
docker exec mansio-postgres psql -q -U mansio -d mansio -c "create schema $schema" >/dev/null
subst() { sed -e "s|@SCHEMA@|$schema|g" -e "s|@PREFIX@|$prefix|g" "$@"; }
subst "$root/configs/soak/controld.json" > "$out/controld.json"
subst "$root/configs/soak/edge.json" > "$out/edge.json"
jq -n --arg pg "$pg_dsn" --arg c "$control_token" --arg a "$admin_token" --arg e "$edge_token" \
  '{pg_dsn:$pg, control_token:$c, admin_token:$a, edge_token:$e, s3_access_key:"gc", s3_secret_key:"gc-secret-dev"}' \
  > "$out/controld-secrets.json"
printf '%s' "$edge_token" > "$out/edge-token"
chmod 600 "$out/controld-secrets.json" "$out/edge-token"
ports_vm_a="17070 17071 17072" ports_vm_b="17073 17074 17075"
for vm in vm-a vm-b; do
  ports="ports_${vm/-/_}"; read -r q h a <<< "${!ports}"
  subst -e "s|@VM@|$vm|g" -e "s|@QPORT@|$q|g" -e "s|@HPORT@|$h|g" -e "s|@APORT@|$a|g" \
    "$root/configs/soak/agentd.json" > "$out/$vm/agentd.json"
  jq -n --arg pg "$pg_dsn" --arg c "$control_token" \
    '{s3_access_key:"agent", s3_secret_key:"agent-secret-dev", control_token:$c, pg_dsn:$pg}' > "$out/$vm/secrets.json"
  chmod 600 "$out/$vm/secrets.json"
done

# --- Processes. ----------------------------------------------------------------------------------------------
# start NAME DIR CMD...: runs in the background, pid in $out/NAME.pid, stderr appended to $out/NAME.stderr.
start() {
  local name=$1 dir=$2; shift 2
  (cd "$dir" && exec "$@") 2>> "$out/$name.stderr" > /dev/null &
  echo $! > "$out/$name.pid"
}
stop_proc() { # SIGTERM, wait up to 60 s, then SIGKILL
  local pid; pid="$(cat "$out/$1.pid" 2>/dev/null)" || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for _ in $(seq 600); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.1; done
  log "$1 did not stop in 60 s; killing"; kill -KILL "$pid" 2>/dev/null || true
}
wait_ready() { # URL [curl args]
  local url=$1; shift
  for _ in $(seq 300); do curl -sf -o /dev/null "$@" "$url" && return 0; sleep 0.2; done
  log "not ready: $url"; return 1
}
start_agent() {
  start "$1" "$out/$1" "$bin/agentd" -dev -config "$out/$1/agentd.json" -addr-file "$out/$1/addr"
}
cleanup() {
  set +e
  [ -n "${sampler:-}" ] && kill "$sampler" 2>/dev/null
  [ -n "${faults:-}" ] && kill "$faults" 2>/dev/null
  for p in edge vm-a vm-b controld; do stop_proc "$p"; done
  docker unpause coldload-seaweedfs >/dev/null 2>&1
}
trap cleanup EXIT

log "starting controld (schema $schema, prefix $prefix)"
start controld "$out" "$bin/controld" -dev -config "$out/controld.json"
wait_ready https://127.0.0.1:17080/readyz --cacert "$ca"
start_agent vm-a
start_agent vm-b
wait_ready http://127.0.0.1:17071/readyz
wait_ready http://127.0.0.1:17074/readyz
start edge "$out" "$bin/edge" -dev -config "$out/edge.json"
wait_ready http://127.0.0.1:17444/readyz
mark start ready

# --- Sampler and fault injector. ----------------------------------------------------------------------------
OUT="$out" CA="$ca" PREFIX="$prefix" SAMPLE_EVERY="$SAMPLE_EVERY" "$root/scripts/soak/sample.sh" &
sampler=$!

export MANSIO_ADMIN_TOKEN="$admin_token"
fb=("$bin/frontbench" locator -locator https://127.0.0.1:17080 -ca "$ca" -edge https://127.0.0.1:17443
  -tenant fb -dbs "$DBS" -zipf "$ZIPF" -write "$WRITE" -timeout 10s -warmup 0s
  -ack-log "$out/acks.tsv" -events "$out/events.jsonl")
log "setup: $DBS databases"
MANSIO_API_KEY= "${fb[@]}" -setup -duration 1s -conc 1 -rate 1 -out "$out/setup.json" > "$out/setup.txt" 2>&1 || { cat "$out/setup.txt"; exit 1; }
api_key="$(curl -sf --cacert "$ca" -X POST -H "Authorization: Bearer $admin_token" \
  https://127.0.0.1:17080/v1/tenants/fb/api-keys -d '{"principal":"soak"}' | jq -r .key)"
export MANSIO_API_KEY="$api_key"

fault_windows() {
  local t0=$1
  at() { local s=$(( t0 + $1 - $(date +%s) )); [ "$s" -gt 0 ] && sleep "$s"; return 0; }
  act() { "$@" >> "$out/faults.log" 2>&1 || log "fault action failed: $*"; }
  at "$F_BREAKER_AT"; mark fault_start s3_breaker_vm-a
  act "$bin/mansioctl" -agent 127.0.0.1:17072 agent breaker s3 trip
  at $(( F_BREAKER_AT + F_BREAKER_FOR ))
  act "$bin/mansioctl" -agent 127.0.0.1:17072 agent breaker s3 reset; mark fault_end s3_breaker_vm-a
  # S3 silent: the container is paused (accepts nothing, answers nothing: "accept, no reply" from the
  # client's view of a hung server). Pause, not stop: a stopped container that compose created from a
  # worktree since removed cannot start again (its bind mount is gone).
  at "$F_S3_AT"; mark fault_start s3_paused
  act docker pause coldload-seaweedfs
  at $(( F_S3_AT + F_S3_FOR ))
  act docker unpause coldload-seaweedfs; mark fault_end s3_paused
  at "$F_RESTART_AT"; mark fault_start restart_vm-b
  stop_proc vm-b; start_agent vm-b
  wait_ready http://127.0.0.1:17074/readyz || true; mark fault_end restart_vm-b
  at "$F_PG_AT"; mark fault_start pg_breaker_vm-b
  act "$bin/mansioctl" -agent 127.0.0.1:17075 agent breaker placement trip
  at $(( F_PG_AT + F_PG_FOR ))
  act "$bin/mansioctl" -agent 127.0.0.1:17075 agent breaker placement reset; mark fault_end pg_breaker_vm-b
}

phase() { # NAME SECONDS RATE CONC MOVE (0 seconds: skipped)
  [ "$2" -gt 0 ] || return 0
  mark phase_start "$1"
  "${fb[@]}" -run-id "$1" -duration "${2}s" -rate "$3" -conc "$4" -move-every "$5" -seed "$RANDOM" \
    -out "$out/$1.json" >> "$out/frontbench.txt" 2>&1 || log "frontbench $1 exited $?"
  mark phase_end "$1"
}

if [ "${NOFAULTS:-0}" != 1 ]; then fault_windows "$(date +%s)" & faults=$!; fi
phase load1 "$LOAD1" "$RATE" "$CONC" "$MOVE_EVERY"
[ -n "${faults:-}" ] && { wait "$faults" || true; }; faults=
phase idle "$IDLE" "$IDLE_RATE" 1 0
phase load2 "$LOAD2" "$RATE" "$CONC" "$MOVE_EVERY"
sleep 30 # let write-backs settle before verifying
mark verify start
"$bin/frontbench" verify -locator https://127.0.0.1:17080 -ca "$ca" -edge https://127.0.0.1:17443 -tenant fb \
  -dbs "$DBS" -ack-logs "$out/acks.tsv" -out "$out/verify.json" | tee -a "$out/run.log" || log "verify FAILED"
mark verify end
sleep "$SAMPLE_EVERY"
kill "$sampler" 2>/dev/null || true; sampler=
for p in edge vm-a vm-b controld; do stop_proc "$p"; done
mark stop done
trap - EXIT

if [ "${KEEP:-0}" != 1 ]; then
  aws --endpoint-url http://localhost:8333 s3 rm --recursive --quiet "s3://coldload/$prefix" || true
  docker exec mansio-postgres psql -q -U mansio -d mansio -c "drop schema $schema cascade" >/dev/null || true
fi
"$root/scripts/soak/analyze.sh" "$out" > "$out/analysis.txt" 2>&1 || log "analysis failed (see analysis.txt)"
log "results in $out"
