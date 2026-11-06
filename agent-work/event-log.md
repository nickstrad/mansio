# Shared agent event log

## Location and durability

The live inbox is `$(git rev-parse --path-format=absolute --git-common-dir)/agent-work/events/`. Every linked
worktree resolves the same directory, regardless of branch or agent family. A plain worktree-relative
`agent-work/events/` would split shared state between worktrees; a hard-coded main checkout would couple
writers to another agent's checkout. The Git common directory avoids both. Separate clones and other VMs do
**not** share an inbox; they consume pushed archives.

Tracked `agent-work/events/` holds the [schema](events/README.md) and batched copies of events. The live inbox
is authoritative for current VM activity; archives provide Git durability. Writers never stage, commit, or
push. One coordinator snapshots and commits at the end of each work batch/session, before worktree/VM cleanup.
It may also snapshot during its 20-minute status run. Unarchived events survive session exit and branch
switches, but not deletion of the common Git directory or loss of the VM. No background service or cron is
installed by this change.

## Publication and concurrency

The helper validates the closed six-type schema before writing. It creates a unique `.pending.*` directory
inside the inbox with `mktemp`, writes the complete event there, and publishes with `ln -T` to a
timestamp/actor/random filename. Hard-link creation on the same local filesystem is atomic and refuses an
existing destination. Readers see a whole event or nothing; simultaneous writers cannot interleave or
overwrite. A collision fails visibly; retry creates a new suffix. No shared counter, append stream, lock, or
Git index is involved. Dependencies are Bash, Git, and GNU coreutils (the VM's standard tools). Run from a
worktree in the target repository.

Only timestamp-named `*.md` files are events. Readers ignore hidden temporary directories. Normal exit and
INT/TERM clean the writer's temporary directory; SIGKILL may leave hidden scratch, which is harmless. Do not
clean another running writer's scratch. Publication is not an fsync/power-loss guarantee. If a client loses
its connection after publication, read the inbox before retrying: retries are distinct events, not
automatically deduplicated. Treat files as immutable; corrections reference prior filenames.

## Write protocol (Claude and Codex)

Use a distinct actor per newly spawned fresh-context unit, stable only for that unit, and the task ID in every
summary. Do not reuse an actor or completed session for another unit. `task_started` may report a resume after
an interruption or unblock only for the same still-active unit; after `task_completed`, the next unit requires
a new agent, worktree, and actor under the
[fresh-agent lifecycle](../AGENTS.md#fresh-agent-lifecycle-every-unit-of-work). Log before starting, after
finishing, at review verdicts/verified landings, and immediately when blocked or needing a decision. A
review's detail names the exact reviewed SHA; a completion names validation and build SHA. Approval and
landing rules still come from `AGENTS.md`.

From this branch's checkout on the VM:

```bash
cd /root/Software/mansio-work/eventlog
export AGENT_ACTOR=codex-eventlog
work-scripts/bash/log-event.sh --type task_started --branch infra/event-log \
  --summary "Event-log implementation started"
work-scripts/bash/log-event.sh --type review_verdict --branch infra/event-log \
  --actor opus-review-eventlog --summary "PASS event-log review" \
  --detail "Reviewed SHA: replace-with-reviewed-sha; include checks and findings."
```

The second command illustrates syntax; emit PASS only after an actual review. After landing, use the helper
from your own checkout (or an absolute path to a checkout containing it while keeping your cwd in the target
repo). Neither Claude nor Codex needs its own integration. Through the host SSH wrapper:

```bash
~/workspace/goals/claude-code-and-codex-sessions-on-vm-1/hidden_files/bin/sshvm.sh 'cd /root/Software/mansio-work/eventlog && work-scripts/bash/log-event.sh --type task_started --branch infra/event-log --actor claude-eventlog --summary "Event-log implementation started" --detail "Session: eventlog; task: centralized agent reporting."'
```

These are ordinary Bash commands with no model-specific environment. Replace the actor/task/branch and
checkout for real work. Outer single quotes protect the remote command; do not interpolate arbitrary untrusted
prose into a shell command. Locally, quote arguments and pass multiline detail as `--detail "$detail"`.
Success prints the published absolute filename; nonzero exit is a logging failure that the caller must
surface, not silently ignore.

## Read protocol

There is no shared file to `tail -f`. List event filenames in lexical order and tail that list, then read the
selected files. Same-second ordering is arbitrary; filenames are IDs, not a causal sequence. From any linked
worktree, this prints the latest 20 complete events:

```bash
export LC_ALL=C
inbox="$(git rev-parse --path-format=absolute --git-common-dir)/agent-work/events"
shopt -s nullglob
files=("$inbox"/[0-9]*.md)
if ((${#files[@]})); then
  printf '%s\n' "${files[@]}" | tail -n 20 | while IFS= read -r file; do
    printf '\n%s\n' "${file##*/}"
    cat -- "$file"
  done
fi
```

Filter exact header fields, never body text. Set any filter to empty to accept all values; run after the setup
above. For pushed historical events, set `inbox=agent-work/events` instead.

```bash
want_type=blocker
want_actor=
want_branch=infra/event-log
for file in "$inbox"/[0-9]*.md; do
  mapfile -t header < <(head -n 9 -- "$file")
  [[ -z $want_type || ${header[7]} == "type: $want_type" ]] || continue
  [[ -z $want_actor || ${header[5]} == "actor: $want_actor" ]] || continue
  [[ -z $want_branch || ${header[6]} == "branch: $want_branch" ]] || continue
  printf '\n%s\n' "${file##*/}"
  cat -- "$file"
done
```

### 20-minute status consumer

Each run scans **all** live filenames and compares them with its own acknowledged IDs. Do not use a "last
timestamp" cursor or just the latest 20: delayed publications and same-second writers would be missed. Include
new starts/completions/landings/verdicts in the status report; carry unresolved blockers and decisions forward
until an event explicitly resolves them. No new events means "no new reports", not proof a task is idle or
completed. Reconcile branch SHAs before declaring a landing.

The existing status cron can use this selection snippet; `seen` belongs to that single consumer, outside the
immutable events. Keep cron invocations serialized by its scheduler. Process/report each event successfully
before executing the acknowledgement line. A crash may repeat an event (at-least-once), but does not skip one.
Retain filenames in the report for deduplication.

```bash
common="$(git rev-parse --path-format=absolute --git-common-dir)"
inbox=$common/agent-work/events
seen=$common/agent-work/status-20min-seen
mkdir -p -- "$seen"
shopt -s nullglob
for file in "$inbox"/[0-9]*.md; do
  id=${file##*/}
  [[ ! -e $seen/$id ]] || continue
  cat -- "$file"
  # Integrate report delivery here; on failure, exit without acknowledging.
  # After confirmed delivery: touch -- "$seen/$id"
done
```

## Archive protocol (one coordinator)

In the coordinator's own clean archive branch/worktree, snapshot complete events. Do not run these commands in
another agent's checkout. This build does not activate shared writes or archival outside its assigned
worktree; activation follows coordinator adoption. The example commits on the current branch and never merges
main. Use the usual reviewed landing process for that archive branch.

```bash
set -euo pipefail
inbox="$(git rev-parse --path-format=absolute --git-common-dir)/agent-work/events"
mkdir -p agent-work/events
pending=$(mktemp -d agent-work/events/.archive.XXXXXXXXXXXX)
trap 'rm -rf -- "$pending"' EXIT
shopt -s nullglob
for file in "$inbox"/[0-9]*.md; do
  target=agent-work/events/${file##*/}
  if [[ -e $target ]]; then
    cmp -- "$file" "$target" # Same ID must always mean identical bytes.
  else
    cp -- "$file" "$pending/event"
    ln -T -- "$pending/event" "$target"
    rm -- "$pending/event"
  fi
done
# Start with a clean index so the commit contains only this archive batch.
archives=(agent-work/events/[0-9]*.md)
if ((${#archives[@]})); then git add -- "${archives[@]}"; fi
if ! git diff --cached --quiet -- agent-work/events; then
  git commit -m "docs: archive agent events"
  git push origin HEAD
fi
```

Concurrent writers during the snapshot are fine: later files are picked up next batch. Do not remove live
events after archiving; local readers retain one continuous history. In a fresh clone, the archive is readable
immediately; before starting live writers, the coordinator may seed its empty live inbox with the timestamped
archive files. Existing IDs must be byte-identical, never overwritten. Do not edit or reformat published event
files: the first-line `deno-fmt-ignore-file` directive tells Deno to leave their bytes intact, including
arbitrary Markdown detail. Format the schema/design docs normally.
