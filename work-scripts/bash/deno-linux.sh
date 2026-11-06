#!/bin/sh
# Run a deno command for this checkout inside Linux (denoland/deno:2.9.5 + libsqlite3).
#
# Why: platform/sqlite_ffi refuses darwin/aarch64 (variadic sqlite3_db_config; docs/knowledge/gotchas.md),
# so every SQLite-backed test fails on Apple silicon. This runs the same command in an arm64 Linux container.
# node_modules and DENO_DIR live in named volumes, so the host's macOS node_modules is never touched.
#
# usage: work-scripts/bash/deno-linux.sh 'deno task ci'          (from the checkout root)
set -eu
root=$(git rev-parse --show-toplevel)
tag=$(basename "$root" | tr -c 'A-Za-z0-9_.\n-' '-')
exec docker run --rm -v "$root":/work -v "mansio-nm-$tag":/work/node_modules -v mansio-deno-dir:/deno-dir \
  -e DENO_DIR=/deno-dir -w /work denoland/deno:2.9.5 \
  sh -c "apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq libsqlite3-0 >/dev/null 2>&1; $1"
