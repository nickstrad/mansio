#!/usr/bin/env bash
# Usage: scripts/lock.sh <lock> <command> [args...]
#
# Runs a command while holding a lock, so two agents never write the same file (or run benchmarks) at once.
# <lock> is a name (lock dir .locks/<name> at the repo root) or a path containing a slash (used as given, e.g.
# results/.bench-lock, which coldload's run.sh also uses). The lock is a directory: mkdir is atomic.
# It waits while another live process holds the lock; a lock whose owner pid is gone is removed as stale.
# The owner file reads "<who> pid <pid> started <time>: <command>"; set LOCK_WHO to name yourself.
set -euo pipefail
[ $# -ge 2 ] || { sed -n '2,10p' "$0" >&2; exit 2; }
root="$(cd "$(dirname "$0")/.." && pwd)"
name="$1"; shift
case "$name" in */*) lock="$root/$name" ;; *) lock="$root/.locks/$name" ;; esac
mkdir -p "$(dirname "$lock")"
waited=0
until mkdir "$lock" 2>/dev/null; do
  owner="$(cat "$lock/owner" 2>/dev/null || true)"
  pid="$(printf '%s' "$owner" | sed -n 's/.*pid \([0-9][0-9]*\).*/\1/p')"
  if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
    echo "lock.sh: removing stale lock $lock (owner gone: $owner)" >&2
    rm -rf "$lock"
    continue
  fi
  [ $((waited % 10)) -eq 0 ] && echo "lock.sh: waiting for $lock held by: ${owner:-unknown}" >&2
  sleep 1; waited=$((waited + 1))
done
printf '%s pid %s started %s: %s\n' "${LOCK_WHO:-$(whoami)}" "$$" "$(date '+%F %T')" "$*" > "$lock/owner"
trap 'rm -rf "$lock"' EXIT INT TERM
"$@"
