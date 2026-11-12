#!/usr/bin/env bash
# claude-limit.sh — scrape the Claude Code weekly usage limit from the TUI.
# The CLI has no flag for this; the interactive /status Usage tab is the only
# source. Runs claude in a throwaway tmux session, navigates to the Usage tab,
# prints a compact line, and cleans up.
#
# Output example:
#   weekly_limit_pct_used=60 weekly_limit_pct_left=40 weekly_resets="Oct 13, 5:59am (UTC)"
set -euo pipefail

S="claude-limit-$$"
cleanup() { tmux kill-session -t "$S" 2>/dev/null || true; }
trap cleanup EXIT

tmux kill-session -t "$S" 2>/dev/null || true
tmux new-session -d -s "$S" -x 120 -y 40 "claude" >/dev/null

# wait for the TUI to finish loading (up to 60s)
loaded=0
for _ in $(seq 1 30); do
  if tmux capture-pane -t "$S" -p 2>/dev/null | grep -q "Claude Code v"; then
    loaded=1
    break
  fi
  sleep 2
done
if [ "$loaded" -ne 1 ]; then
  echo "ERROR: claude TUI did not load" >&2
  exit 1
fi

tmux send-keys -t "$S" "/status" Enter
sleep 6

# navigate tabs until the Usage tab's "Current week" section appears
found=0
for _ in $(seq 1 6); do
  if tmux capture-pane -t "$S" -p 2>/dev/null | grep -q "Current week (all models)"; then
    found=1
    break
  fi
  tmux send-keys -t "$S" Right
  sleep 3
done
if [ "$found" -ne 1 ]; then
  echo "ERROR: could not reach Usage tab" >&2
  exit 1
fi

pane=$(tmux capture-pane -t "$S" -p 2>/dev/null)
pct_used=$(printf '%s' "$pane" | grep -A1 "Current week (all models)" | grep -oE '[0-9]+% used' | head -n 1 | grep -oE '[0-9]+')
resets=$(printf '%s' "$pane" | grep -A2 "Current week (all models)" | grep -oE 'Resets [^(]+\(UTC\)' | head -n 1 | sed 's/^Resets //')

if [ -z "${pct_used:-}" ]; then
  echo "ERROR: could not parse usage percent" >&2
  exit 1
fi
pct_left=$((100 - pct_used))
echo "weekly_limit_pct_used=${pct_used} weekly_limit_pct_left=${pct_left} weekly_resets=\"${resets:-unknown}\""
