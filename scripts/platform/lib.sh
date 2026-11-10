# shellcheck shell=bash disable=SC2034 # its variables are used by the scripts that source it
# Shared by scripts/platform/up.sh and down.sh (sourced; expects $root). Not executable on its own.

state="${PLATFORM_DIR:-$root/.platform}" # everything platform-up generates; down.sh deletes it
state="$(realpath -m -- "$state")" # absolute (the scripts cd before they use it) and one spelling per directory
# The Compose project, one per state directory (so per checkout): mansio-platform-<8 hex of the path's sha256>.
# `make platform-down` in one checkout then removes its own containers and volumes, never those of a platform
# started from another checkout. The same name is the marker file up.sh leaves in $state.
project="mansio-platform-$(printf '%s' "$state" | sha256sum | cut -c1-8)"
marker="$state/.$project"
pg_port="${PLATFORM_PG_PORT:-15432}"
s3_port="${PLATFORM_S3_PORT:-18333}"
pg_schema=control
# The dev stack's bucket and identity: the same as `make s3-init` and the root docker-compose.yml (public test
# values, DEV ONLY). Deliberately not AWS_*: real credentials in the environment are never used.
s3_bucket=mansio-test
s3_access_key=mansiodevkey
s3_secret_key=mansiodevsecret
daemons=(controld vm-a vm-b edge)
export PATH="$HOME/.deno/bin:$PATH"

log() { echo "platform: $*" >&2; }
die() { log "error: $*"; exit 1; }

compose() { # compose.yml takes its project name from PLATFORM_PROJECT
  PLATFORM_PROJECT="$project" PLATFORM_PG_PORT="$pg_port" PLATFORM_S3_PORT="$s3_port" \
    docker compose -f "$root/scripts/platform/compose.yml" "$@"
}

port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# start NAME BIN ARGS...: detached from this shell (its own session, so `make` returning does not stop it);
# pid in $state/NAME.pid, stderr in NAME/stderr.log (the daemon's JSON log is NAME/<daemon>.log).
start() {
  local name=$1 dir="$state/$1"; shift
  (cd "$dir" && exec setsid "$@") < /dev/null > /dev/null 2>> "$dir/stderr.log" &
  echo $! > "$state/$name.pid"
}

# The pid of NAME if it is alive and is still the binary platform-up started (not a reused pid).
live_pid() {
  local pid exe
  pid="$(cat "$state/$1.pid" 2>/dev/null)" || return 1
  exe="$(readlink "/proc/$pid/exe" 2>/dev/null)" || return 1
  case "$exe" in "$state/bin/"*) echo "$pid" ;; *) return 1 ;; esac
}

all_running() {
  local d
  for d in "${daemons[@]}"; do live_pid "$d" >/dev/null || return 1; done
}

wait_ready() { # NAME URL [curl args]: 60 s, failing early if the process exits
  local name=$1 url=$2; shift 2
  for _ in $(seq 300); do
    curl -sf -o /dev/null "$@" "$url" && return 0
    live_pid "$name" >/dev/null || die "$name exited before it was ready"
    sleep 0.2
  done
  die "$name not ready after 60 s ($url)"
}

stop_proc() { # SIGTERM, wait up to 30 s, then SIGKILL
  local pid
  pid="$(live_pid "$1")" || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for _ in $(seq 300); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.1; done
  log "$1 did not stop in 30 s; killing"
  kill -KILL "$pid" 2>/dev/null || true
}

tails() { # the last lines of every daemon's logs, for a failed run
  local d f
  for d in "${daemons[@]}"; do
    for f in "$state/$d/"*.log; do
      [ -s "$f" ] && { echo "--- $f" >&2; tail -n 20 "$f" >&2; }
    done
  done
  return 0
}

smoke() {
  log "smoke: schema, tenant and key, one SDK write and one SDK read through the edge"
  (cd "$root" && DENO_CERT="$state/tls/ca.pem" PLATFORM_DIR="$state" deno task -q platform:smoke)
  # Independent of the SDK's word: the edge's own count of agent answers it relayed.
  log "edge relayed $(curl -sf http://127.0.0.1:27444/metrics | awk '/^mansio_edge_agent_answers_total\{status="2xx"\}/ {print $2}') 2xx agent answers since it started"
}
