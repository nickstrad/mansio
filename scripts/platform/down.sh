#!/usr/bin/env bash
# Usage: scripts/platform/down.sh   (make platform-down)
#
# Removes what scripts/platform/up.sh created and nothing else: stops the daemons it started (pid files in
# .platform/, each checked to still be one of .platform/bin's binaries), removes the `mansio-platform` Compose
# project's containers, network and volumes, and deletes .platform/ (binaries, dev CA, ticket keys, secrets,
# configs, caches, logs). The root docker-compose.yml stack (`make up`) and its volumes are not touched.
# Safe to run when nothing is up.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=scripts/platform/lib.sh
source "$root/scripts/platform/lib.sh"

if [ -d "$state" ]; then
  [ -f "$state/.mansio-platform" ] || die "$state has no .mansio-platform marker; not deleting a directory platform-up did not create"
  for d in edge vm-a vm-b controld; do stop_proc "$d"; done
fi
if command -v docker >/dev/null 2>&1; then
  log "removing compose project $project (containers, network, volumes)"
  compose down -v --remove-orphans >&2 2>&1
fi
if [ -d "$state" ]; then
  rm -rf "$state"
  log "removed $state"
fi
log "platform down"
