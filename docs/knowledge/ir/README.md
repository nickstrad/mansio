# ir

The IR protocol: `/v1/ops` requests (queries and atomic mutations with preconditions) that the SDK sends and
any backend executes. Spec: [`ir/spec/v1.md`](../../../ir/spec/v1.md); plan:
[`docs/plans/ir-protocol.md`](../../plans/ir-protocol.md); vectors:
[`ir/vectors/`](../../../ir/vectors/README.md). Code: `core/ir/` (decode, canonical JSON, wire errors,
execution rules), `platform/ir/executor.ts` (the reference executor on SQLite).

Script: `work-scripts/bash/deno-linux.sh '<deno command>'` runs SQLite-backed tests from an Apple-silicon Mac.

## Entries (newest first)

- [2026-11-05 IR-0 reference executor](2026-11-05-ir-0-reference-executor.md): how the reference executor
  differs from today's mutation path, where the protocol's open questions were settled, and why SQLite tests
  need Linux on Apple silicon.
