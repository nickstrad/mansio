#!/usr/bin/env bash
set -euo pipefail

# Isolated Git fixtures only; never writes the real shared inbox or another worktree.
root=$(git rev-parse --show-toplevel)
logger=$root/agent-work/work-scripts/bash/log-event.sh
mkdir -p "$root/tmp"
scratch=$(mktemp -d "$root/tmp/event-log-check.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
export LC_ALL=C
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE

git init -q --initial-branch=fixture "$scratch/repo"
cd "$scratch/repo"
git -c user.name=Fixture -c user.email=fixture@example.invalid commit -q --allow-empty -m fixture
git worktree add -q -b fixture-linked "$scratch/linked"
inbox=$(git rev-parse --path-format=absolute --git-common-dir)/agent-work/events

reject() {
  if "$logger" "$@" > "$scratch/out" 2> "$scratch/err"; then
    printf 'Unexpected success: %s\n' "$*" >&2
    exit 1
  fi
}
reject --type unknown --branch fixture --summary invalid
reject --type task_started --branch fixture --summary $'two\nlines'
reject --type task_started --branch fixture --summary '   '
reject --type task_started --branch $'bad\rbranch' --summary invalid
reject --type task_started --branch fixture --summary invalid --actor ../escape
reject --type task_started --branch fixture --summary invalid --detail
reject --type task_started --branch fixture --summary invalid --unknown x
[[ ! -e $inbox ]]

for type in task_started task_completed landed review_verdict blocker decision_needed; do
  AGENT_ACTOR=fixture-env "$logger" --type "$type" --branch fixture --summary "Type: $type" >/dev/null
done

# All contenders share actor and branch, including two different worktrees.
pids=()
for ((i=1; i<=100; i++)); do
  (
    if ((i % 2)); then cd "$scratch/linked"; fi
    "$logger" --type task_completed --branch fixture --actor same-actor \
      --summary "Concurrent $i" --detail $'type: blocker\nComplete body.' > "$scratch/path-$i"
  ) &
  pids+=("$!")
done
# Any visible event must already have the complete fixed header and detail body.
shopt -s nullglob
for ((pass=0; pass<10; pass++)); do
  for file in "$inbox"/[0-9]*.md; do
    mapfile -t lines < "$file"
    [[ ${lines[0]} == '<!-- deno-fmt-ignore-file -->' && ${lines[1]} == '# Agent event' && ${lines[3]} == '```text' && ${lines[9]} == '```' ]]
    if [[ ${lines[5]} == 'actor: same-actor' ]]; then
      [[ ${#lines[@]} == 13 && ${lines[12]} == 'Complete body.' ]]
    fi
  done
done
for pid in "${pids[@]}"; do wait "$pid"; done
files=("$inbox"/[0-9]*.md)
[[ ${#files[@]} == 106 ]]
for ((i=1; i<=100; i++)); do
  IFS= read -r file < "$scratch/path-$i"
  [[ $file == "$inbox/"* ]]
  mapfile -t lines < "$file"
  [[ ${lines[8]} == "summary: Concurrent $i" && ${lines[7]} == 'type: task_completed' ]]
done
pending=("$inbox"/.pending.*)
[[ ${#pending[@]} == 0 ]]

# A quote, dollar sign, or colon is data; no eval/YAML interpretation.
file=$("$logger" --type decision_needed --branch fixture --actor literal --summary 'Literal: "$(false)"')
mapfile -t lines < "$file"
[[ ${lines[8]} == 'summary: Literal: "$(false)"' ]]
# Plain Bash command transport as used by sshvm.sh (wrapper runs on the host).
bash -c 'cd "$1" && "$2" --type task_started --branch fixture --actor remote --summary "SSH command syntax"' bash "$scratch/linked" "$logger" >/dev/null
printf '%s\n' 'PASS: validation, six types, 100 concurrent cross-worktree writes, complete publication, literal fields, SSH command syntax'
