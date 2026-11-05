# docs/state_files — archived agent state files

When an agent finishes a workstream it moves its live state file (`STATE-<name>.md` at the repo root) here,
adds an "ARCHIVED <date>" note at the top, and posts a line in `shared_state.md`. The files stay as a record of
goals, design decisions, status history and findings. Findings meant to last also live in `knowledge/`.

| File | Workstream | Archived |
|---|---|---|
| `STATE-coldload.md` | coldload: SQLite files in SeaweedFS (S3) → virtual actors; fetch/open/warm timings | 2026-11-04 |
| `STATE-writeback.md` | writeback: warm → cold durability (generations, conditional manifest, write-back, eviction) | 2026-11-05 |
