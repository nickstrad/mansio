/**
 * Runs the pure read-path compiler (`core/plan.ts`, `core/compile/select.ts`, `core/compile/decode.ts`) on a
 * set of sample plans against `examples/tic-tac-toe/schema.ts`. Prints each step's real output, so explanations of the
 * compiler can quote real output instead of guessing.
 *
 * Sections: (1) select-all walkthrough: normalise, compileSelect in both dialects, decode one row;
 * (2) tableFor; (3) compileSelect variations per dialect: filters, null, sort, count, countBy, `json`
 * layout; (4) failure cases, including which error wins when a plan is wrong twice.
 *
 * No database, network, or file writes: it only imports repo modules.
 * Usage: deno run --config deno.json work-scripts/deno/compile-plan-probe.ts [section]
 *   section: walk | table | variants | failures (default: all)
 * Knowledge entries that use it: docs/knowledge/compiler/.
 */
import { Either } from "effect";
import { schema as appSchema } from "../../examples/tic-tac-toe/schema.ts";
import { type Schema, withStorage } from "../../core/schema.ts";
import { normalise, type SelectPlan } from "../../core/plan.ts";
import { compileSelect, tableFor } from "../../core/compile/select.ts";
import { decode } from "../../core/compile/decode.ts";

// The SDK builder and the core descriptor are structurally identical; erase the SDK's typing.
const schema = appSchema as unknown as Schema;
// Plans built by hand here may be invalid on purpose, so the builder takes loose input.
const plan = (extra: Record<string, unknown> = {}): SelectPlan =>
  ({ kind: "select", table: "games", limit: 100, filters: [], ...extra }) as SelectPlan;

const show = (label: string, result: Either.Either<unknown, unknown>) =>
  console.log(
    label.padEnd(26),
    Either.isRight(result) ? JSON.stringify(result.right) : "LEFT " + JSON.stringify(result.left),
  );

const sections: Record<string, () => void> = {
  walk() {
    const base = plan();
    show("normalise", normalise(base));
    show("compileSelect sqlite", compileSelect(schema, base, "sqlite"));
    show("compileSelect duckdb", compileSelect(schema, base, "duckdb"));
    // A raw row as the FFI binding returns it: INTEGER columns as bigint, the array column as JSON text.
    const row = {
      _id: "g1",
      _creationTime: 1730800000000n,
      _version: 1n,
      xPlayer: "alice",
      oPlayer: "bob",
      board: '[null,null,null,null,"X",null,null,null,null]',
      turn: "O",
      status: "playing",
      winner: null,
    };
    show("decode", decode(schema, base, [row]));
  },
  table() {
    show("tableFor games", tableFor(schema, "games"));
    show("tableFor gamez", tableFor(schema, "gamez"));
    // Inherited Object.prototype names must not count as collections (Object.hasOwn).
    show("tableFor toString", tableFor(schema, "toString"));
    show("tableFor __proto__", tableFor(schema, "__proto__"));
  },
  variants() {
    const json = withStorage(schema, "json");
    const moves = {
      table: "moves",
      filters: [
        { field: "gameId", op: "eq", value: "g1" },
        { field: "moveNumber", op: "gt", value: 3 },
      ],
    };
    for (const dialect of ["sqlite", "duckdb"] as const) {
      console.log(`-- ${dialect}`);
      show("base", compileSelect(schema, plan(), dialect));
      show(
        "eq status",
        compileSelect(schema, plan({ filters: [{ field: "status", op: "eq", value: "won" }] }), dialect),
      );
      show(
        "eq winner null",
        compileSelect(schema, plan({ filters: [{ field: "winner", op: "eq", value: null }] }), dialect),
      );
      show(
        "2 filters + sort",
        compileSelect(schema, plan({ ...moves, order: { field: "moveNumber", direction: "desc" } }), dialect),
      );
      show(
        "sort _creationTime",
        compileSelect(schema, plan({ order: { field: "_creationTime", direction: "desc" } }), dialect),
      );
      show(
        "count (order dropped)",
        compileSelect(
          schema,
          plan({ aggregate: { kind: "count" }, order: { field: "status", direction: "asc" } }),
          dialect,
        ),
      );
      show(
        "countBy winner",
        compileSelect(schema, plan({ aggregate: { kind: "countBy", field: "winner" } }), dialect),
      );
      show("json layout eq+gt", compileSelect(json, plan(moves), dialect));
    }
  },
  failures() {
    const one = (field: string, op: string, value: unknown) => plan({ filters: [{ field, op, value }] });
    show("unknown field", compileSelect(schema, one("nope", "eq", 1), "sqlite"));
    show("array field", compileSelect(schema, one("board", "eq", []), "sqlite"));
    show("value not in enum", compileSelect(schema, one("status", "eq", "paused"), "sqlite"));
    // Validation runs before the operator checks, so these use valid values to reach them.
    show("gt on enum", compileSelect(schema, one("status", "gt", "won"), "sqlite"));
    show("gt null", compileSelect(schema, one("winner", "gt", null), "sqlite"));
    show("unsupported op", compileSelect(schema, one("status", "like", "won"), "sqlite"));
    show("invalid value beats bad op", compileSelect(schema, one("status", "like", "a"), "sqlite"));
    show(
      "bad direction",
      compileSelect(schema, plan({ order: { field: "status", direction: "up" } }), "sqlite"),
    );
    show(
      "order by array",
      compileSelect(schema, plan({ order: { field: "board", direction: "asc" } }), "sqlite"),
    );
    // Check order: table lookup, then limit, then filters.
    show("bad table + bad limit", compileSelect(schema, plan({ table: "gamez", limit: 0 }), "sqlite"));
    show(
      "bad limit + bad field",
      compileSelect(schema, plan({ limit: 0, filters: [{ field: "nope", op: "eq", value: 1 }] }), "sqlite"),
    );
  },
};

const pick = Deno.args[0];
if (pick !== undefined && !Object.hasOwn(sections, pick)) {
  console.error(`unknown section ${pick}; one of: ${Object.keys(sections).join(", ")}`);
  Deno.exit(2);
}
for (const [name, run] of Object.entries(sections)) {
  if (pick !== undefined && name !== pick) continue;
  console.log(`== ${name}`);
  run();
}
