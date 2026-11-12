#!/usr/bin/env bash
# Usage: scripts/shared-note.sh <agent> <topic> < body.md
#
# Appends an entry to shared_state.md (the append-only log between agents) while holding the "shared_state"
# lock, so concurrent appends never interleave. The heading is "## <date time> — <agent>: <topic>".
set -euo pipefail
[ $# -eq 2 ] || { sed -n '2,6p' "$0" >&2; exit 2; }
root="$(cd "$(dirname "$0")/.." && pwd)"
body="$(cat)"
LOCK_WHO="$1" "$root/scripts/lock.sh" shared_state bash -c '
  printf "\n## %s — %s: %s\n\n%s\n" "$(date "+%F %H:%M")" "$1" "$2" "$3" >> "$4"' _ "$1" "$2" "$body" "$root/shared_state.md"
