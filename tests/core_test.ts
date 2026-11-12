/**
 * The pure compiler regressions from the original 20 tests (plan item 20): one logical plan through both
 * dialects and both storage layouts, typed rejections instead of JS-scan fallbacks, numeric casts in the
 * JSON layout, BIGINT decoding and the DDL's indexes.
 *
 * The storage-dependent originals and the `StorageLive`/`StorageTest` conformance suite ran against the
 * Deno platform's SQLite writer and were removed with it (IR-6); the Go server's conformance is the e2e
 * suite and `server/`'s own tests.
 */
import assert from "node:assert/strict";
import { test } from "node:test";
import { Effect } from "effect";
import { CompileError } from "../core/errors.ts";
import { compileDDL } from "../core/compile/ddl.ts";
import { compileSelect } from "../core/compile/select.ts";
import { decode } from "../core/compile/decode.ts";
import { Documents, type SelectPlan } from "../sdk/db.ts";
import { withStorage } from "../sdk/schema.ts";
import { schema } from "../examples/tic-tac-toe/schema.ts";

test(
  "same logical plan generates both dialects and both storage layouts",
  () =>
    Effect.runPromise(Effect.gen(function* () {
      let plan: SelectPlan | undefined;
      const db = new Documents<typeof schema>((op) =>
        Effect.sync(() => {
          assert.equal(op.kind, "select");
          plan = op as SelectPlan;
          return [];
        })
      );
      yield* db.find("games", { status: "playing" }).sort("_creationTime", "desc").limit(20).collect();
      const columnSQL = yield* compileSelect(schema, plan!, "duckdb");
      const jsonSQL = yield* compileSelect(withStorage(schema, "json"), plan!, "duckdb");
      const sqliteSQL = yield* compileSelect(withStorage(schema, "json"), plan!, "sqlite");
      assert.match(columnSQL.text, /FROM "app"\."games" WHERE "status" = \$p1/);
      assert.match(jsonSQL.text, /json_extract_string\("doc", '\$\.status'\)/);
      assert.match(sqliteSQL.text, /json_extract\("doc", '\$\.status'\)/);
      assert.deepEqual(columnSQL.values, ["playing", 20]);
      assert.deepEqual(jsonSQL.values, columnSQL.values);
    })),
);

test(
  "array predicates and unknown fields fail instead of falling back to JS scans",
  () =>
    Effect.runPromise(Effect.gen(function* () {
      const base = { kind: "select" as const, table: "games", limit: 20 };
      const array = yield* Effect.flip(
        compileSelect(schema, { ...base, filters: [{ field: "board", op: "eq", value: [] }] }, "duckdb"),
      );
      assert.ok(array instanceof CompileError);
      assert.equal(array.reason, "array-predicate");
      const unknown = yield* Effect.flip(
        compileSelect(schema, { ...base, filters: [{ field: "notAField", op: "eq", value: "x" }] }, "sqlite"),
      );
      assert.ok(unknown instanceof CompileError);
      assert.equal(unknown.reason, "unknown-field");
      assert.equal(unknown.field, "notAField");
    })),
);

test(
  "JSON numeric queries cast extracted values; null uses IS NULL",
  () =>
    Effect.runPromise(Effect.gen(function* () {
      const json = withStorage(schema, "json");
      const numeric = yield* compileSelect(json, {
        kind: "select",
        table: "moves",
        limit: 9,
        filters: [{ field: "moveNumber", op: "gt", value: 3 }],
      }, "duckdb");
      assert.match(numeric.text, /CAST\(json_extract_string\("doc", '\$\.moveNumber'\) AS BIGINT\) > \$p1/);
      const nullable = yield* compileSelect(json, {
        kind: "select",
        table: "games",
        limit: 20,
        filters: [{ field: "winner", op: "eq", value: null }],
      }, "duckdb");
      assert.match(nullable.text, /IS NULL/);
      assert.deepEqual(nullable.values, [20]);
    })),
);

test(
  "decoder normalizes DuckDB BIGINT results to safe JS numbers",
  () =>
    Effect.runPromise(Effect.gen(function* () {
      const plan = { kind: "select" as const, table: "games", filters: [], limit: 20 };
      const rows = (yield* decode(schema, plan, [{
        _id: "g",
        _creationTime: 1700000000000n,
        _version: 1n,
        xPlayer: "alice",
        oPlayer: "bob",
        board: JSON.stringify(Array(9).fill(null)),
        turn: "X",
        status: "playing",
        winner: null,
      }])) as { _creationTime: number; board: unknown[] }[];
      assert.equal(typeof rows[0]._creationTime, "number");
      assert.equal(rows[0].board.length, 9);
    })),
);

test(
  "schema compiler emits indexes for either physical layout",
  () =>
    Effect.runPromise(Effect.gen(function* () {
      assert.match((yield* compileDDL(schema)).join("\n"), /"status" TEXT NOT NULL/);
      const json = (yield* compileDDL(withStorage(schema, "json"))).join("\n");
      assert.match(json, /"doc" TEXT NOT NULL CHECK/);
      assert.match(json, /CREATE INDEX.*json_extract/);
    })),
);
