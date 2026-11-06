# IR test vectors

The cross-language contract for [IR protocol v1](../spec/v1.md) (ADR 0010). TypeScript runs them in
`tests/unit/ir_vectors_test.ts` (`deno task test`); the Go runner (mansio-server IR-1/IR-2) reads the same
files. Every bug fix in either implementation adds a vector.

**Do not edit the JSON by hand.** The source is `scripts/ir/gen-vectors.ts`; regenerate with

```sh
deno run -A scripts/ir/gen-vectors.ts && deno fmt ir/
deno run -A scripts/ir/gen-compile-corpus.ts          # generated/compile-corpus.jsonl
```

The TS runner fails if a committed file differs from its generator. `ir/spec/v1.schema.json` is informative
only: it is not run against the vectors, and cannot express several decode rules (see its description).
Expectations are hand-written in the source, except compiled SQL (`compile.json`, the corpus) and schema
fingerprints (`schema.json`, `execute.json`'s `fingerprint`), which are golden output of `core/` — the oracle
by decision.

## Files

| File                             | Vectors | Input → expected                                                                          |
| -------------------------------- | ------- | ----------------------------------------------------------------------------------------- |
| `decode.json`                    | 95      | request bytes → accepted (+ canonical encoding) or error                                  |
| `validation.json`                | 39      | table fields + body (insert = full, patch = partial) → validated body or error            |
| `schema.json`                    | 21      | registered schema JSON → fingerprint text and `sha256:` fingerprint, or declaration error |
| `compile.json`                   | 70      | schema + plan / write / DDL → SQLite SQL text and binds, or compile error (both layouts)  |
| `execute.json`                   | 122     | schema + seed rows + requests (fixed clock, ids) → per-step result or error, final rows   |
| `generated/compile-corpus.jsonl` | 2000    | fast-check schemas + plans → SQL or error, for differential testing                       |

`execute.json` holds 61 scenarios, each once per storage layout (`[columns]`, `[json]` in the name).

## Common shape

Each `.json` file is

```json
{ "category": "decode", "version": 1, "description": "…", "vectors": [ { "name": "…", "why": "…", … } ] }
```

`name` is unique within the file; `why` says what the vector pins. Strings are JSON strings: `"\u0000"` in a
vector is one U+0000 character.

**Schemas** (`schema`, `fields`) are in the registered form of the spec §2, i.e. `JSON.stringify` of a
`core/schema.ts` schema. Load them with the declaration rules (`decodeSchema`). The vector schema has three
tables: `notes` (one field of every kind: `title` string, `score` number, `rank` integer, `done` boolean,
`tag` nullable enum, `tags` string array, `grid` integer array of length 3, `owner` nullable string; index
`by_rank`), `lists` (`name`) and `entries` (`listId`, `n`; index `by_list`).

**Errors** are `{"error": {"tag", "status", "op"?, "fields"?}}`, compared with the wire error (spec §8):

- `tag` and `status` exactly;
- `op` exactly: absent in the vector means absent in the response;
- `fields` as a **subset**: every member listed must deep-equal the response's `fields` member; unlisted
  members are not compared (e.g. `expected` wording, `actual` values in decode errors).

`compile.json` and the corpus are the exception: their errors are `{"error": {"tag", "fields"}}` with the
complete payload of the `core/` error (no status: they are compiler outputs, not responses), compared exactly.

## `decode.json`

| Member   | Meaning                                                                                               |
| -------- | ----------------------------------------------------------------------------------------------------- |
| `raw`    | the exact body text; its UTF-8 bytes are the request                                                  |
| `base64` | instead of `raw`: the exact body bytes (used for invalid UTF-8)                                       |
| `pad`    | optional: append ASCII spaces (0x20) until the body is exactly `pad` bytes (the 64 KiB on/off points) |
| `expect` | `{"ok": true, "canonical": "<RFC 8785 text of the decoded request>"}` or an error                     |

Decode only: no database, no fingerprint check. Accepted requests use the placeholder fingerprint
`sha256:000…0`.

## `validation.json`

`fields` (the `notes` field map), `mode` (`insert` → full `parseObject`; `patch` → `partial = true`), `value`;
`expect` is `{"ok": true, "value": <validated body>}` or an error. Validation includes the U+0000 storage rule
(spec §5.2). Errors have no `op`.

## `schema.json`

`schema` → `{"ok": true, "text": "<fingerprint text>", "fingerprint": "sha256:…"}`, or an error. A schema is
accepted only if it decodes, passes the declaration rules and its DDL compiles (`empty-enum` is a DDL error).

## `compile.json`

`schema`, `kind`, and per kind:

| `kind`    | Inputs                                            | `expect`                      | TS function                             |
| --------- | ------------------------------------------------- | ----------------------------- | --------------------------------------- |
| `ddl`     | —                                                 | `{"statements": [text…]}`     | `compileDDL(schema)`                    |
| `select`  | `plan` (a `SelectPlan`, with `kind`)              | `{"sql": {"text", "values"}}` | `compileSelect(schema, plan, "sqlite")` |
| `insert`  | `table`, `value`, `meta` (`_id`, `_creationTime`) | `{"sql": …}`                  | `compileInsert`                         |
| `replace` | `table`, `id`, `value` (a full body)              | `{"sql": …}`                  | `compileReplace`                        |
| `delete`  | `table`, `id`                                     | `{"sql": …}`                  | `compileDelete`                         |

SQL text must match byte for byte; `values` are the positional binds in order (booleans already 0/1, arrays
already JSON text).

## `execute.json`

| Member        | Meaning                                                                                                                                                                                                                                                                                                                                                                                      |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `schema`      | the database's schema (one storage layout)                                                                                                                                                                                                                                                                                                                                                   |
| `fingerprint` | its `sha256:` fingerprint; requests carry it in `schema` unless the vector tests a mismatch                                                                                                                                                                                                                                                                                                  |
| `seed`        | `{table: [doc…]}` written before the first step, bypassing the protocol: each doc's `_id`, `_creationTime` and `_version` are stored as given                                                                                                                                                                                                                                                |
| `steps`       | requests run in order against the same database                                                                                                                                                                                                                                                                                                                                              |
| `final`       | `{table: [doc…]}` — every row of each listed table after the last step, ordered by `_id`; **an unlisted table must be empty**. Optional `requests`: `[{request_id, applied_ms, result_hex}]`, the whole `_mansio_requests` table ordered by `request_id`, `result_hex` the lowercase hex of the stored blob (`02` + canonical JSON); compared exactly when present, not compared when absent |

Each step: `clock` (the request's single clock reading, ms), `serverIds` (the ids the server's generator
returns, in order, for inserts without a client id; using more, or fewer, is a runner failure), `request` (the
envelope, which runs through decode first), and `expect`: `{"result": [...]}` plus `"duplicate": true` when
the request id was already applied, or an error.

Documents compare as JSON values regardless of member order; arrays compare in order. The runner may seed and
dump the database through its own connection; in TS both go through `core/compile`.

## `generated/compile-corpus.jsonl`

For differential testing of the Go compiler (plan §6.2). Line 1 is a header:

```json
{ "format": "mansio/ir-compile-corpus", "version": 1, "seed": 20261105, "count": 2000, "dialect": "sqlite" }
```

Every other line is one case:

```json
{ "i": 0, "family": "valid" | "defect" | "junk", "schema": {…}, "plan": {…},
  "select": { "sql": { "text", "values" } } | { "error": { "tag", "fields" } },
  "ddl": { "statements": [ … ] } | { "error": { "tag", "fields" } } }
```

- `schema` is in the registered form but is **not** declaration-checked: load it straight into tables (`junk`
  cases have illegal names, empty enums, itemless arrays, odd lengths). The compiler must be total over it.
- `plan` is a `SelectPlan` (with `kind: "select"`) whose `filters[].op`, `order.direction` and `limit` may be
  anything; it has not been through IR decode.
- `select` is `compileSelect(schema, plan, "sqlite")`; `ddl` is `compileDDL(schema)`. Both are compared
  exactly, including the error's complete `fields` (`reason`, `table`, `field`, `detail`, `path`, `expected`,
  `actual`).
- Inputs were JSON round-tripped before compiling, so the expectation is about exactly these bytes.

The case list is a pure function of `(count, seed)`; the TS runner regenerates it and requires the file to
match. For larger local runs: `deno run -A scripts/ir/gen-compile-corpus.ts 1000000 <seed> /tmp/corpus.jsonl`.
