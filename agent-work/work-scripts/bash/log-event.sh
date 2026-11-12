#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' 'Usage: log-event.sh --type <type> --branch <branch> --summary "<one line>" [--actor <id>] [--detail "<more>"]'
}
fail() { printf 'log-event: %s\n' "$*" >&2; exit 1; }

type= branch= summary= detail=
actor=${AGENT_ACTOR:-session-$$}
while (($#)); do
  case $1 in
    --help|-h) usage; exit 0 ;;
    --type|--branch|--summary|--actor|--detail)
      (($# >= 2)) || fail "Missing value for $1"
      case $1 in
        --type) type=$2 ;;
        --branch) branch=$2 ;;
        --summary) summary=$2 ;;
        --actor) actor=$2 ;;
        --detail) detail=$2 ;;
      esac
      shift 2 ;;
    *) usage >&2; fail "Unknown argument: $1" ;;
  esac
done
case $type in
  task_started|task_completed|landed|review_verdict|blocker|decision_needed) ;;
  *) fail 'Invalid or missing --type (see agent-work/events/README.md)' ;;
esac
export LC_ALL=C
[[ $actor =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}$ ]] || fail 'Actor must be 1-80 ASCII letters, digits, dots, underscores or hyphens; start with a letter/digit'
[[ -n $branch && ! $branch =~ [[:cntrl:]] ]] || fail 'Branch must be nonempty with no control characters'
[[ $summary =~ [^[:space:]] && ! $summary =~ [[:cntrl:]] ]] || fail 'Summary must be one nonblank line with no control characters'

# Resolve from the caller's worktree, not the script's location.
common=$(git rev-parse --path-format=absolute --git-common-dir) || fail 'Run inside the target Git repository'
events=$common/agent-work/events
mkdir -p -- "$events"
# Reserve a unique temporary directory on the same filesystem. Only *.md are events.
pending=$(mktemp -d "$events/.pending.XXXXXXXXXXXX")
trap 'rm -rf -- "$pending"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
name=${stamp//:/-}-$actor-${pending##*.}.md
{
  printf '<!-- deno-fmt-ignore-file -->\n# Agent event\n\n```text\n'
  printf 'timestamp: %s\nactor: %s\nbranch: %s\ntype: %s\nsummary: %s\n' "$stamp" "$actor" "$branch" "$type" "$summary"
  printf '```\n'
  if [[ -n $detail ]]; then printf '\n%s\n' "$detail"; fi
} > "$pending/event"
# A hard link is atomic and fails if the destination exists: never overwrite.
ln -T -- "$pending/event" "$events/$name"
printf '%s\n' "$events/$name"
