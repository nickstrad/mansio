# ADR 0005 — Text storage contract: U+0000 is rejected before commit

- **Status:** accepted
- **Date:** 2026-10-11
- **Deciders:** the plan at `docs/plans/completed/effect-port-and-observability.md` item 31, from the accepted
  `reflection/astra` P1 finding ("resolve or explicitly reject embedded-NUL text"); recorded by item 31
- **Related:** [ADR 0002 — error taxonomy](0002-error-taxonomy.md)
- **Implements:** `core/schema.ts` (`NUL`, `storable`, the enum-value check in `defineTable`),
  `platform/sqlite.ts` (the Storage boundary checks and the stored-text probe),
  `tests/layers/storage_test_layer.ts` (the same contract in `StorageTest`)

## Context

Item 13's SQLite oracle found that text containing U+0000 does not survive the writer. Measured on this
droplet (Deno 2.9.5, `node:sqlite`, DuckDB 1.5.6 for the reader):

| Path                                        | What happens to `"a\0b"`                                           |
| ------------------------------------------- | ------------------------------------------------------------------ |
| `node:sqlite` bind (insert, filter, id)     | Bound intact. SQLite stores all three bytes; `_id = ?` matches.    |
| `node:sqlite` read of a TEXT column         | **Truncated** at the first NUL: `"a"`. No error.                   |
| `json` layout `doc` column (JSON text)      | `JSON.stringify` escapes it as `\u0000`; reads back intact.        |
| `json_extract(doc, '$.s')` on SQLite        | Decodes the escape into raw TEXT, which the driver then truncates. |
| `CAST(x AS BLOB)`, `instr(x, char(0))`      | See every byte. `json_tree(...).atom` holds the NUL too.           |
| DuckDB reader over the attached SQLite file | Lossless: `_id`, columns and `json_extract_string` keep the NUL.   |

So an accepted value could commit and then read back as a different string — `_id: "\0"` came back as `""` —
with nothing failing. The writer's reads matter: a mutation reads through the writer (`get`, `find`, `count`,
`countBy`, and the read inside every `patch`), and `patch` writes the merged document back, so a truncated
read would be committed again as truth.

The two options the plan allows:

1. **Lossless driver reads.** Read every text column as `CAST(... AS BLOB)` and decode UTF-8 in `decode`. That
   changes the SQL `core/compile/select.ts` emits for every read (and `core/compile/GENERATED_SQL.txt`),
   changes the row shape `decode` and `StorageTest` agree on, and still leaves SQLite's own text functions,
   which the SQLite documentation says behave unpredictably on embedded NULs. It is the larger change, in
   files this item does not own, for a character no document in this repo needs.
2. **Typed rejection before commit.** Refuse U+0000 wherever text enters SQLite, with the existing error
   payloads, and make every read of an older stored NUL fail instead of truncating.

## Decision

**Typed rejection.** Storage never accepts text containing U+0000, and never returns a truncated value.

### 1. What is rejected, and with what

Every text value a mutation hands `Storage` is checked before the statement that would carry it runs. Both
physical layouts and both `Storage` implementations (`StorageLive`, `StorageTest`) apply the same rules:

| Where the text is                                                                       | Failure                                                                                    |
| --------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| A `string` or `enum` field value, or an array item at any depth, in `insert` or `patch` | `SchemaError { reason: "type-mismatch", path, expected: "text without U+0000", actual }`   |
| The id given to `get`, `patch` or `delete`                                              | the same `SchemaError`, `path: "_id"`                                                      |
| Any filter value in a read (`find(...).where`, `get`)                                   | the same `SchemaError`, `path` = the filter's field                                        |
| An id produced by `IdGen` for an `insert`                                               | `StorageError { operation: "insert", table, detail }` — a platform fault, not the caller's |

`path` follows `validate`'s convention: the field name, `tags[2]` for an array item, `grid[1][0]` for nested
arrays. The check runs after the existing compile and validation steps, so a value that is wrong in two ways
still reports the old failure first, and before any SQL runs, so a rejected operation writes nothing.

A rejection is an ordinary typed failure of the operation. If the handler lets it propagate, the whole
mutation rolls back, including writes that ran before it. If the handler catches it, the rejected operation
has written nothing and the rest of the mutation commits as usual.

### 2. What was already stored fails explicitly

A database written before this contract can hold NUL text. Every writer read whose result carries such text
fails with `StorageError { operation: "select", table, detail }`, which names the column. That covers `_id`,
`string`/`enum` columns, array columns, the `json` layout's `doc`, and `countBy` group values. Live `Storage`
detects it with a second statement over the same compiled read (`instr(col, char(0))`, and `json_tree` for
JSON-bearing columns). The probe runs only when the read returned rows that carry text, and it is ordered
after the read and before `decode`. `count` carries no text and is unaffected. Rows without NUL in the same
table read normally. Because ids containing NUL can no longer be named, such a row cannot be patched or
deleted through the API; removing it takes manual SQL.

### 3. What is deliberately not covered

- **`validate` is unchanged.** It decides whether a value has its node's type, and it also validates handler
  arguments and reader filters, where a NUL never reaches `node:sqlite`. Handler arguments may contain U+0000.
  Only storing one, or naming one in a lookup, is refused. `core/schema.ts` exports the check as `storable`.
- **Handler results.** A mutation's return value is JSON-encoded for transport, never written to SQLite. JSON
  carries U+0000 intact, so there is nothing to enforce.
- **The reader.** Queries run on DuckDB, which reads the SQLite file losslessly (table above). A query filter
  containing U+0000 is not rejected. Older stored NUL text comes back unchanged: `_id`, `countBy` values and
  `string` fields alike, since `decode` re-validates the body with the unchanged `validate`. Only an `enum`
  value holding a NUL, which can never be a declared member, is refused there (`SchemaError`). In no case does
  the reader return a truncated success.
- **Lone surrogates.** A string with an unpaired UTF-16 surrogate (`"a\uD800b"`) is still accepted and, in the
  `columns` layout, reads back with U+FFFD in its place, because `node:sqlite` binds text as UTF-8. That is a
  separate, known divergence (`tests/layers/storage_test_layer.ts`, "known divergence 3"; pinned in
  `tests/core_test.ts`). This contract covers U+0000 only.
- **Schema names.** Table, field and index names keep their explicit identifier rule
  (`/^[A-Za-z_][A-Za-z0-9_]*$/`), which already excludes U+0000. `defineTable` now also refuses an enum that
  declares a member containing U+0000 (`SchemaError`, reason `type-mismatch`, expected
  `enum values without
  U+0000`): such a member could never be stored, and its DDL `CHECK` literal would
  truncate in SQLite.

## Consequences

- No accepted string or id is changed by U+0000 truncation: it reads back exactly as written, or the write is
  refused. (Lone surrogates are the one remaining exception; see above.)
- Stored NUL text that used to be readable becomes unreadable through `Storage`: `string`/`enum` columns,
  array columns and `json`-layout bodies that previously returned (possibly truncated) text now fail the read
  with `StorageError` instead, and a `patch` of such a row fails the same way.
- Clients that need NUL bytes must encode them (base64, or an array of code points) before storing.
- A writer read that returns text runs one more statement over the same compiled read, which emits its own
  `sql` event. Results are small (`get` is one row; `find` is capped at 1000), but the probe re-runs the whole
  read: an indexed lookup for `get` and indexed filters, a full rescan of the table for `find` or `countBy` on
  unindexed fields.
- The oracle's documents still exclude U+0000, because the oracle checks compiled SQL rather than Storage. The
  NUL regression goes through the real `StorageLive` instead.
- Lossless reads remain possible later: a compiler item could emit `CAST(... AS BLOB)` and lift this
  restriction. It would need a new ADR that supersedes this one.
