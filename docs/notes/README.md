# Notes

Durable notes on how parts of mansio work, why they were built that way, and the traps found while building
them. These cover the TypeScript side (`core/`, `sdk/`) and the IR protocol end to end; notes on the Go
server's internals are in [`server/docs/notes/`](../../server/docs/notes/README.md). Plans (`docs/plans/`),
architecture (`docs/architecture/`) and ADRs (`docs/adr/`) are authoritative for their details; notes record
the how, the why and the traps, and link to them.

- [Decisions](decisions.md): architectural and process decisions with dates and reasons.
- [Gotchas](gotchas.md): environment and tooling traps.
- [Compiler](compiler/README.md): how plans and schemas become SQL (`core/compile/`), the oracle for the Go
  port.
  - [How `compileSelect` turns a read plan into SQL](compiler/compile-select.md): check order, placeholder
    numbering, per-dialect field access, and refusals.
- [Intermediate representation](ir/README.md): the IR protocol (`/v1/ops`), its spec and vectors, the SDK that
  speaks it, and the Go server features around it.
  - [The SDK runs handlers and talks `/v1/ops`](ir/sdk-handlers.md): derived preconditions, handler retry,
    transport rules, e2e.
  - [The read path](ir/read-path.md): one snapshot, guarded finds, conflict state, cached reads, and the
    sequence after a restore.
  - [Tenancy and auth on agentd](ir/tenancy-and-auth.md): agentd's auth port, admin and sign-in API, Better
    Auth scrypt, `controld import-mansio`.
  - [Backup, restore and import](ir/backup-restore-import.md): backups as forced write-backs, `dbtool restore`
    and `dbtool import`.
  - [agentd's event log](ir/event-log.md): the JSONL records agentd writes and what they mean.
  - [The IR on a cluster](ir/multi-node.md): auth at resolve, the edge, `scale_out`, and the Jepsen-style run.

Scale-out design (what limits mansio past one process and machine, and the Go server that replaced the Deno
platform) is in [`docs/architecture/`](../architecture/README.md), starting with
[the Go greenfield proposal](../architecture/scale-out-go-greenfield.md) and
[ADR 0013](../adr/0013-go-server-replaces-deno-platform.md).
