# Table lookup and `compileSelect` in depth

- **Date:** 2026-11-05
- **Question:** What exactly happens in `tableFor` and `compileSelect` (steps 3 and 4 of the
  [select-all walkthrough](2026-11-05-select-all-walkthrough.md)), and how does each part of a plan change the
  SQL?
- **Read at:** `422ac91`
- **Evidence:** every SQL string and error below is real output from
  `deno run --config deno.json agent-work/work-scripts/deno/compile-plan-probe.ts` (sections `table`,
  `variants`, `failures`). Line references are from reading `core/compile/select.ts`,
  `core/compile/dialect.ts`, `core/plan.ts` and `core/schema.ts`.

## How the code is written: `Either.gen`

Every function here returns `Either<Value, Error>` and never throws. Inside `Either.gen(function* () { … })`,
`yield*` on an `Either` either unwraps the `Right` value or stops the whole function and returns that `Left`.
So `compileSelect` reads top to bottom as a list of checks, and **the first check that fails is the error you
get**. The order of the `yield*` lines decides which error is reported.

## Step 3: `tableFor` (`select.ts:31`)

```ts
if (!Object.hasOwn(schema, name)) {
  return Either.left(new CompileError({ reason: "unknown-collection", table: name }));
}
return Either.right(schema[name]);
```

The schema is a plain object, `{ games: Table, moves: Table }`. Each `Table` is plain data that the SDK
builders (`defineTable`, `v.*`) made from `examples/tic-tac-toe/schema.ts`:

```
games: Table
├── storage: "columns"          ← one SQLite column per field ("json" = one `doc` TEXT column)
├── indexes: [{ name: "by_status", fields: ["status"] }]
└── fields:
    ├── xPlayer  { kind: "string" }
    ├── oPlayer  { kind: "string" }
    ├── board    { kind: "array", length: 9, item: { kind: "enum", values: [X,O], nullable } }
    ├── turn     { kind: "enum", values: [X,O] }
    ├── status   { kind: "enum", values: [playing, won, draw] }
    └── winner   { kind: "enum", values: [X,O], nullable: true }
```

The returned `Table` drives every later decision: which fields exist, their kinds, and the storage layout.

`Object.hasOwn`, not `name in schema`, is the part that matters. Inherited names don't count as tables:

```
tableFor "games"     → Right(Table)
tableFor "gamez"     → Left unknown-collection
tableFor "toString"  → Left unknown-collection     ("toString" in schema would be true)
tableFor "__proto__" → Left unknown-collection
```

Field lookup (`fieldNode`, `select.ts:40`) uses the same `hasOwn` rule. It checks three hidden system columns
first: `_id` (string), `_creationTime` (integer) and `_version` (integer). They are real columns in both
layouts, so they are never read out of the JSON.

## Step 4: `compileSelect` (`select.ts:149`), in check order

```
① tableFor(schema, plan.table)        unknown-collection
② normalise(plan)                     invalid-limit; drops `order` when there is an aggregate
③ for each filter:
     a. scalar(table, field)          unknown-field | array-predicate
     b. validate(node, value)         SchemaError type-mismatch
     c. expression(table, field, d)   how to read the field in this layout and dialect
     d. eq + null → "expr IS NULL"    (no bind value)
     e. op not eq/gt                  unsupported-operator
     f. gt on non-number, or gt null  gt-requires-numeric
     g. push "expr = ?" / "expr > ?"  bind(value)
④ quote(table) → target               "games" (sqlite) | "app"."games" (duckdb)
⑤ projection                          * | COUNT(*) AS "count" | <expr> AS "value", COUNT(*) AS "count"
⑥ ORDER BY                            invalid-ordering | array-predicate
⑦ LIMIT                               bound, except for count
```

Check order, shown with plans that are wrong twice:

```
bad table + limit 0          → unknown-collection   (① before ②)
limit 0 + unknown field      → invalid-limit        (② before ③)
op "like" + value "a"        → type-mismatch        (③b before ③e: the value is checked first)
op "like" + value "won"      → unsupported-operator
```

The `select.ts` header says this order matches the pre-port compiler on purpose, so the same bad plan reports
the same error it always did.

### The `bind` closure: how placeholders are numbered

```ts
const values: Bind[] = [];
const bind = (value) => {
  values.push(value);
  return placeholder(dialect, values.length);
};
```

Each call adds the value to `values` and returns a placeholder for its position: `?` for SQLite, `$p1`, `$p2`,
… for DuckDB. Because `LIMIT` is bound last, it always gets the last number:

```
filters gameId="g1", moveNumber>3, LIMIT 100
  values: ["g1", 3, 100]
  sqlite: … "gameId" = ?   AND "moveNumber" > ?   … LIMIT ?
  duckdb: … "gameId" = $p1 AND "moveNumber" > $p2 … LIMIT $p3
```

User values never appear in the SQL text. Only schema names do, and those go through `quote`, which checks
`^[A-Za-z_][A-Za-z0-9_]*$` and wraps the name in `"…"` (`dialect.ts:37`). `encode` (`select.ts:126`) turns a
value into a bind value: booleans become `1`/`0`, arrays become JSON text, and null stays null.

### `expression`: how a field is read (`select.ts:104`)

This is where the storage layouts and dialects differ. The same field, `moveNumber` (an integer):

```
columns layout:        "moveNumber"
json layout, sqlite:   json_extract("doc", '$.moveNumber')
json layout, duckdb:   CAST(json_extract_string("doc", '$.moveNumber') AS BIGINT)
```

- SQLite's `json_extract` returns the JSON's own type, so integers, strings and booleans need no cast.
  `number` fields are the exception and get `CAST(… AS REAL)`. A large integral double is stored as integer
  text, and SQLite would read it back as an exact INTEGER that no longer equals the double.
- DuckDB's `json_extract_string` always returns text, so it casts to `BIGINT`, `DOUBLE`, or `BOOLEAN` then
  `INTEGER`. Without the cast, `moveNumber > 3` would compare strings.
- The same function builds index keys in `ddl.ts`. That way, a `json`-layout index matches the WHERE
  expression exactly and SQLite's planner can use it.

### How each part of a plan changes the SQL (SQLite)

```
{}                                  SELECT * FROM "games" ORDER BY "_id" ASC LIMIT ?
where status = "won"                SELECT * FROM "games" WHERE "status" = ? ORDER BY "_id" ASC LIMIT ?
where winner = null                 SELECT * FROM "games" WHERE "winner" IS NULL ORDER BY "_id" ASC LIMIT ?
sort _creationTime desc             SELECT * FROM "games" ORDER BY "_creationTime" DESC NULLS LAST, "_id" ASC LIMIT ?
count (+ a sort, dropped)           SELECT COUNT(*) AS "count" FROM "games"
countBy winner                      SELECT "winner" AS "value", COUNT(*) AS "count" FROM "games"
                                      GROUP BY 1 ORDER BY "value" ASC NULLS FIRST LIMIT ?
```

Rules visible above:

- **`= null` becomes `IS NULL`**, because `x = NULL` is never true in SQL. No bind value is added.
- **There is always an ORDER BY.** With no sort, it orders by `"_id"`. With a sort, `"_id" ASC` is the
  tie-breaker. So the same data always comes back in the same order, and a page boundary never moves.
- **NULLs go first when ascending and last when descending.** SQLite and DuckDB have different defaults for
  this, so writing it out makes both engines agree.
- **`count` has no LIMIT.** It counts every match. `countBy` limits the number of groups, not the rows read.
- Several filters are joined with `AND`. Only `eq` and `gt` exist.

### What it refuses

```
unknown field "nope"        → CompileError unknown-field
filter or sort on "board"   → CompileError array-predicate   (arrays are JSON text; comparing them compares text)
status = "paused"           → SchemaError  type-mismatch      (not in the enum)
status > "won"              → CompileError gt-requires-numeric
winner > null               → CompileError gt-requires-numeric
op "like"                   → CompileError unsupported-operator
direction "up"              → CompileError invalid-ordering
```

The SDK's TypeScript types already stop most of these in app code (`ScalarKey`, `NumericKey` in `sdk/db.ts`).
The runtime checks are the second line of defence, for plans that come through `as` casts, JavaScript, or a
future wire format. _Unverified:_ whether any current caller can actually send a plan the types would reject.
