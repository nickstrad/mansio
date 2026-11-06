# ADR 0011 — `(tenant, app)` identifies a database; client ids are UUIDv7

- **Status:** accepted
- **Date:** 2026-11-05
- **Deciders:** [`docs/plans/ir-protocol.md`](../plans/ir-protocol.md) §9; recorded by IR-0
- **Related:** [ADR 0009](0009-ir-is-the-app-wire.md)
- **Implements:** `ir/spec/v1.md` §4 and §5.3, `core/ir/decode.ts`

## Decision

1. **A database is named by `(tenant, app)`**, carried in every request's `db` member. controld maps the pair
   to a database key; one SQLite file holds one app's tables for one tenant. Request ids, document ids and
   versions are scoped to that database.
2. **Client-generated document ids are UUIDv7**, in canonical lowercase form (`xxxxxxxx-xxxx-7xxx-Vxxx-…`,
   variant `8`–`b`). An insert may carry one; the server checks the syntax and that the id is unused in the
   table (an implicit `expect: {exists: false}`, `ConflictError` otherwise). Without one the server draws an
   opaque id.

## Why

- `(tenant, app)` is what callers already know; a separate database id would be one more mapping to keep.
- UUIDv7 ids sort by creation time (good B-tree locality for `_id` primary keys), need no coordination, so
  offline inserts and `$ref`-free retries keep their ids, and one fixed spelling means one id cannot exist
  twice under two cases.

## Consequences

- The database key is derived, not mapped: `(tenant, app)` is the control plane's registry row
  `tenant ":" app` (mansio-server CP-4), so `tenant` and `app` are the registry's lowercase domains
  (`ir/spec/v1.md` §4), narrower than wire ids. Resolving `{"tenant", "app"}` and the raw `{"tenant", "db"}`
  reach the same key space.
- Ids issued by the server before this decision (UUIDv4 from `IdGenLive`, `id-N` in tests) stay valid ids: the
  UUIDv7 rule applies to client-supplied insert ids only. Every other id position accepts any non-empty string
  without U+0000.
