# ir/ — the IR protocol in Go (IR-1: decode, schema, compiler; IR-2b: execution and /v1/ops)

Owner: IR builders (IR-1 Go core; IR-2 adds execution). Plan: `../../../docs/plans/ir-protocol.md`; spec:
`../../../ir/spec/v1.md`; vectors: `../../../ir/vectors/`.

## Notes

- [2026-11-05 IR-2b: docexec and POST /v1/ops](2026-11-05-ir2b-docexec-and-ops-route.md): the executor, the route
  and its error mapping, database creation from the registry, the model fuzz, what IR-3 needs.

## 2026-11-05 — IR-1: Go port of core/ir, core/schema and core/compile (SQLite)

Three pure packages (in the DST-0 lint list, `internal/det/lint_test.go`, now 17 packages):

| Package | Ports | Public API |
|---|---|---|
| `internal/ir` | `core/ir/{json,decode,types,errors}.ts`, `resolve` of `semantics.ts` | JSON values (`nil, bool, float64, string, []any, *Object`, `Undefined{}`; encoders panic on any other Go type); `Parse` (strict), `Stringify`, `StringifyIndent`, `Canonical`, `FormatNumber`, `ToString`; `DecodeBody`, `DecodeValue` → `*Request` with `Op` = `*Select / *Get / *Insert / *Patch / *Delete / *Assert`; `Request.JSON()`; `Resolve`, `ResolveID`; `*Error` (tag, `Fields *Object`, op, `Status()`, `Message()`, `Wire()`; **not** an `error`, see below) |
| `internal/docschema` | `core/schema.ts`, `core/ir/schema.ts` | `Decode` (strict registered JSON + declaration rules), `Load` (no checks, the corpus's `tablesOf`; corpus and fuzz only), `Schema.FingerprintText()`, `Fingerprint`, `Validate`, `Storable`, `ParseObject`, `Body`, `Identifier`, `WithStorage` |
| `internal/doccompile` | `core/compile/*.ts`, `core/plan.ts` normalise | `Select` (SQLite and DuckDB), `DDL`, `Insert`, `Replace`, `Delete`, `Decode` (rows → documents / count / countBy), `PlanOf`, `Args` (bind values as the TS binding types them), `Normalise`, `Expression`, `Scalar`, `FieldNode`, `TableFor`, `Quote` |

What matched (all on 2026-11-05, Apple M-series laptop, go1.26.4):

- Vectors: `decode.json` 95/95, `validation.json` 39/39, `schema.json` 21/21 (fingerprints byte-identical),
  `compile.json` 70/70, committed `compile-corpus.jsonl` 2000/2000 lines byte-identical.
- `GENERATED_SQL.txt` regenerated in Go byte-identical (`TestGeneratedSQL`; inputs in
  `internal/doccompile/testdata/`: `JSON.stringify` of `app/schema.ts` and the SDK plan of `scripts/explain.ts`).
- Differential at scale: 1 000 000 fast-check cases (10 × 100 000, seeds 1–10), every output line
  byte-identical. TS generation 51 s, Go comparison 8 s (`work-scripts/bash/ir-differential.sh`).
- Properties against modernc SQLite: decode∘insert is the identity (both layouts); compiled selects equal a
  reference evaluator (filters, NULL ordering, `_id` tie-break, limit, count, countBy).
- Fuzz: `FuzzDecodeRequest`, `FuzzCanonicalJSON` (`internal/ir`), `FuzzCompile` (`internal/doccompile`),
  seeded from the vectors and corpus. See the IR-1 commit for durations; one harness crasher kept as a
  regression (`testdata/fuzz/FuzzCompile/72f90cbc037f4e39`, below).

TypeScript behaviour reproduced on purpose (each is pinned by a test or the corpus):

1. **Own-property order.** JavaScript iterates array-index names ("0" … "4294967294") first, ascending, then
   insertion order. `ir.Object` keeps that order, so the first unknown member, the first invalid field name
   and the fingerprint text come out as in TS (`TestObjectOrder`, `TestDecodeOrder`).
2. **ECMAScript number text** everywhere a number becomes text: canonical JSON, array binds and `doc` JSON,
   and free-text error fields (`detail: "1..1000, got 1.5"`; a corpus `NaN` limit arrives as `null` and
   reads `got null`). `FormatNumber` uses Go's shortest digits with ECMAScript's notation thresholds.
3. **`String(x)`** for `detail` (`ToString`: arrays join with commas, objects are `[object Object]`).
4. **JSON.stringify escaping**: only `"`, backslash and control characters (lowercase hex); U+2028 and DEL
   are written raw. A lone surrogate (only ever inside an error path) is kept as generalized UTF-8 and
   written back as a backslash-u escape, as JS does.
5. **Parser grammar** is the TS sticky regex: `1.e5` reads `1` then fails at the container; `[1,]` fails at
   path `[1]`; a BOM is an ordinary stray character.
6. **Check order** of the compiler: table, limit, then per filter node → value → expression → `eq null` →
   operator → `gt`; the table name is quoted only after the filters; `invalid-ordering` before the order
   field's expression; insert quotes the table before the columns, replace after.
7. **Junk schemas** (`Load`): absent storage is `columns` (the constructor default); the DDL treats any
   non-`json` storage as columns while `expression` treats any non-`columns` storage as JSON; an empty
   enum fails only in a `columns` table; an absent `values` emits no CHECK; an index with no fields renders
   `ON "t" ()`, which SQLite rejects (TS does the same; only `decodeSchema` refuses empty indexes) — the
   fuzz crasher above was this, fixed in the harness.

Not a quirk: `__proto__` is an ordinary, reserved name. Deno disables the `__proto__` accessor, so
`core/ir/schema.ts`'s `obj[name] = …` stores it as an own property (`TestProtoName`). In Node or a browser
that assignment would set the prototype instead; the SDK computes fingerprints from `defineSchema` values,
not through `decodeSchema`, but keep it in mind if `core/ir/schema.ts` ever runs client-side.

`*ir.Error` deliberately does not implement `error` (2026-11-05 review fixup). Functions return a `*ir.Error`
that is nil on success; were it an `error`, returning that nil through an `error`-typed result would make a
non-nil interface (the typed-nil trap). The compiler now refuses that mix; keep `*ir.Error` until the edge
renders it with `Wire()`, and wrap a non-nil one explicitly if it must travel as an `error`.

Decode of a cap-sized body of array-index member names (`{"0":0,…,"7000":0}`) was quadratic (~615 ms, TS
37 ms): `Object.Set` rescanned the keys. The parser now builds each object in one sorted pass and `Set`
binary-searches its insert: ~1.5 ms in any order (`BenchmarkDecodeIndexNames`, bounded by
`TestIndexNamesDecodeFast`).

Deferred from plan §6.2 (not built in IR-1):

- The differential covers `select` and DDL only. Insert, replace, delete and body validation are covered by
  the hand-written vectors (`compile.json`, `validation.json`), not by a generated corpus.
- No generated corpus for request decode either: `decode.json` plus `FuzzDecodeRequest` (Go only) cover it;
  nothing feeds the same random bytes to the TS decoder.
- The nightly job that runs both compilers' SQL on the same SQLite and compares rows is not built; the Go
  side has `TestSelectMatchesReference` against a Go reference evaluator instead.
- `GENERATED_SQL.txt`'s inputs are frozen JSON in `internal/doccompile/testdata/`; the TS test
  `tests/unit/generated_sql_inputs_test.ts` fails if they (or the committed file) drift from `app/schema.ts`
  and `scripts/explain.ts`.

For IR-2 (`docexec`, `/v1/ops`):

- Write wire output (results, errors, the stored duplicate result) with `ir.Stringify` / `ir.Canonical`,
  never `encoding/json`: it escapes `<`, `>`, `&` and U+2028, formats numbers differently and drops the
  member order.
- Cap the body read at `ir.MaxBodyBytes + 1` bytes before `DecodeBody` (which then answers
  `RequestTooLarge`), so an oversized body is never buffered whole.
- `doccompile.PlanOf` shares the decoded `Filters` slice: resolve `$ref`s into a copy of the filters, do
  not write resolved values back into the request (it may be replayed or compared as a duplicate).

- Decoded values keep their raw `{"$ref": i}` objects; call `ir.Resolve` before validation, as the TS
  executor does. `ir.ResolveID` for id positions.
- Not ported: `checkExpect` (`core/ir/semantics.ts`). Port it on top of `doccompile.Scalar`,
  `docschema.Validate` and `Storable`; the conflict is `ir.NewError(ir.TagConflict, "table", …, "id", …,
  "check", …, "expected", …, "actual", …)`.
- Bind with `doccompile.Args(sql.Values)`; scan rows into `doccompile.Row` (modernc gives int64, float64,
  string, []byte, nil) and decode with `doccompile.Decode`.
- `_mansio_requests.result` is `0x02` + `ir.Canonical(result)`.
- Execute vectors (`execute.json`) are not run in Go yet: they need the executor. (Done in IR-2b:
  `internal/docexec` `TestExecuteVectors`, 122/122.)
