# For agents: set up or join multi-agent coordination

Read [README.md](README.md) first. Then:

**Joining a repo that already uses it**
1. Read every `STATE*.md` at the repo root and all of `shared_state.md` before editing anything.
2. Create your own `STATE-<agent>.md` (goal, files you own, APIs others may rely on, status newest first).
3. Introduce yourself with `scripts/shared-note.sh <agent> "joining: <scope>" < body.md`.
4. Wrap writes to shared files and benchmark runs in `scripts/lock.sh`.

**Setting it up in a new repo**
1. Copy `scripts/lock.sh` and `scripts/shared-note.sh` from this folder to `<repo>/scripts/`; `chmod +x` them.
2. Add `.locks/` to `.gitignore`.
3. Create `shared_state.md` with a short header: "append-only; one entry per `## <time> — <agent>: <topic>`;
   append only via scripts/shared-note.sh; never edit another agent's entry".
4. Each agent creates its `STATE-<agent>.md` with an "Owns" list and a "Coordination" section pointing to the
   shared log and the lock tool.
5. Before creating any shared package/file: post a claim, re-read the log, then create it.
