# ADR 0009 — The IR is the only app wire, and handlers run in the SDK

- **Status:** accepted
- **Date:** 2026-11-05
- **Deciders:** [`docs/plans/ir-protocol.md`](../plans/ir-protocol.md) §9; recorded by IR-0
- **Related:** [ADR 0010](0010-compiler-port-with-ts-oracle.md),
  [ADR 0011](0011-database-identity-and-client-ids.md),
  [`docs/plans/replace-platform-with-go.md`](../plans/replace-platform-with-go.md)
- **Implements:** [`ir/spec/v1.md`](../../ir/spec/v1.md), `core/ir/`, `platform/ir/executor.ts`

## Context

The SDK already describes every database access as a small IR (`SelectPlan`, `insert`, `patch`, `delete`;
`sdk/db.ts`, `core/plan.ts`). Today the Deno server runs the handler that produces it, so any backend must
embed a TypeScript runtime. The Go backend (mansio-server) has actors, routing, durability and request-id
dedupe, but no TypeScript.

## Decision

1. **The IR is the only wire an app uses.** An app sends `POST /v1/ops` with a query (reads) or a mutation (an
   ordered, atomic list of operations with preconditions), as specified in `ir/spec/v1.md`. There is no
   per-handler route and no raw SQL from apps; `/v1/query` (SQL) stays internal, for benches and admin.
2. **Handlers run where the SDK runs.** `defineFunctions` handlers execute in the caller's process. Their
   reads become preconditions (`expect`, `assert`) on the mutation the SDK sends; a conflict comes back as
   `ConflictError` and the SDK re-runs the handler. This is optimistic concurrency.

## Consequences

- The backend validates, compiles and executes IR only; it never runs app code. Any backend that passes the
  shared vectors (`ir/vectors/`) can serve any app.
- Callers are trusted (app backends holding an API key or session). Server-enforced rules for untrusted
  callers are out of scope (plan §8).
- Handlers must be free of side effects beyond `db`: they can run more than once.
- `find` reads inside a handler are unguarded in v1 (no range precondition); v1.1 reserves one.
- The request is a deterministic, idempotent log entry, which keeps local-first sync possible later.
