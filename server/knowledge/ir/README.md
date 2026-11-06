# ir/ — the IR protocol in Go (IR-1: decode, schema, compiler)

Owner: IR builders (IR-1 Go core; IR-2 adds execution). Plan: `../../../docs/plans/ir-protocol.md`; spec:
`../../../ir/spec/v1.md`; vectors: `../../../ir/vectors/`.

## 2026-11-05 — IR-1: Go port of core/ir, core/schema and core/compile (SQLite)

Three pure packages (in the DST-0 lint list, `internal/det/lint_test.go`, now 17 packages):

| Package | Ports | Public API |
|---|---|---|
| `internal/ir` | `core/ir/{json,decode,types,errors}.ts`, `resolve` of `semantics.ts` | JSON values (`nil, bool, float64, string, []any, *Object`, `Undefined`); `Parse` (strict), `Stringify`, `StringifyIndent`, `Canonical`, `FormatNumber`, `ToString`; `DecodeBody`, `DecodeValue` → `*Request` with `Op` = `*Select / *Get / *Insert / *Patch / *Delete / *Assert`; `Request.JSON()`; `Resolve`, `ResolveID`; `*Error` (tag, `Fields *Object`, op, `Status()`, `Wire()`) |
| `internal/docschema` | `core/schema.ts`, `core/ir/schema.ts` | `Decode` (strict registered JSON + declaration rules), `Load` (no checks, the corpus's `tablesOf`), `Schema.FingerprintText()`, `Fingerprint`, `Validate`, `Storable`, `ParseObject`, `Body`, `Identifier`, `DefineTable/DefineSchema/AddIndex`, `WithStorage` |
| `internal/doccompile` | `core/compile/*.ts`, `core/plan.ts` normalise | `Select` (SQLite and DuckDB), `DDL`, `Insert`, `Replace`, `Delete`, `Decode` (rows → documents / count / countBy), `PlanOf`, `Args` (bind values as the TS binding types them) |

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

For IR-2 (`docexec`, `/v1/ops`):

- Decoded values keep their raw `{"$ref": i}` objects; call `ir.Resolve` before validation, as the TS
  executor does. `ir.ResolveID` for id positions.
- Not ported: `checkExpect` (`core/ir/semantics.ts`). Port it on top of `doccompile.Scalar`,
  `docschema.Validate` and `Storable`; the conflict is `ir.NewError(ir.TagConflict, "table", …, "id", …,
  "check", …, "expected", …, "actual", …)`.
- Bind with `doccompile.Args(sql.Values)`; scan rows into `doccompile.Row` (modernc gives int64, float64,
  string, []byte, nil) and decode with `doccompile.Decode`.
- `_mansio_requests.result` is `0x02` + `ir.Canonical(result)`.
- Execute vectors (`execute.json`) are not run in Go yet: they need the executor.
