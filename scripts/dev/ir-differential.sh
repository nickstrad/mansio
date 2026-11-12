#!/usr/bin/env bash
# Differential compiler test at scale (ADR 0010: the TS compiler is the oracle for the Go port): generate a
# large compile corpus with the TypeScript compiler (scripts/ir/gen-compile-corpus.ts,
# in chunks of distinct seeds so no single JS string outgrows V8), then recompile every case with the Go
# compiler (server/internal/doccompile TestCompileCorpus) and require each output line to match byte for byte.
#
# Usage: scripts/dev/ir-differential.sh [total=1000000] [chunk=100000] [first-seed=1] [dir]
# Env:   none required. dir defaults to ${TMPDIR:-/tmp}/mansio-ir-corpus. Chunk files are named by a hash of
#        the TS inputs that decide their content (core/compile, core/plan.ts, core/schema.ts, the generator
#        and its arbitraries), so a file is reused only if those are unchanged; IR_DIFF_REGEN=1 forces it.
# Needs: deno (~/.deno/bin), go. Writes only under dir. Run from anywhere inside the checkout.
set -euo pipefail
export PATH="$HOME/.deno/bin:$PATH"

total=${1:-1000000}
chunk=${2:-100000}
seed=${3:-1}
dir=${4:-${TMPDIR:-/tmp}/mansio-ir-corpus}
root=$(git rev-parse --show-toplevel)
mkdir -p "$dir"
inputs=$(cd "$root" && cat core/compile/*.ts core/plan.ts core/schema.ts core/errors.ts scripts/ir/gen-compile-corpus.ts \
  tests/property/compiler_arbitraries.ts tests/property/arbitraries.ts | git hash-object --stdin | cut -c1-12)

files=()
left=$total
gen_start=$(date +%s)
while [ "$left" -gt 0 ]; do
  n=$((left < chunk ? left : chunk))
  file="$dir/corpus-$inputs-$n-$seed.jsonl"
  if [ "${IR_DIFF_REGEN:-0}" = 1 ] || [ ! -s "$file" ]; then
    (cd "$root" && deno run -A scripts/ir/gen-compile-corpus.ts "$n" "$seed" "$file")
  fi
  files+=("$file")
  left=$((left - n))
  seed=$((seed + 1))
done
gen_end=$(date +%s)
echo "generated/reused $total cases (inputs $inputs) in $((gen_end - gen_start)) s under $dir"

list=$(IFS=,; echo "${files[*]}")
cd "$root/server"
MANSIO_IR_CORPUS="$list" go test ./internal/doccompile -run '^TestCompileCorpus$' -count=1 -v -timeout 2h
