/**
 * The differential compile corpus for the Go compiler port (ADR 0010;
 * `ir/vectors/README.md`, "Generated compile corpus").
 *
 * Draws schemas and select plans from the fast-check arbitraries of `tests/property/` — valid plans,
 * plans with exactly one defect, and junk that ignores every builder invariant — compiles each with
 * `core/compile` (SQLite dialect), and writes one JSON line per case with the SQL and binds or the
 * error. The Go compiler must produce the same line for every case; the Go fuzzer mutates the inputs.
 *
 * Every input is round-tripped through JSON *before* it is compiled, so the expectation is what the
 * compiler says about exactly the bytes Go will read (`NaN` became `null`, `undefined` members
 * vanished, `-0` became `0`).
 *
 * ```sh
 * deno run -A scripts/ir/gen-compile-corpus.ts [count=2000] [seed=20261105] [out=ir/vectors/generated/compile-corpus.jsonl]
 * ```
 */
import fc from "fast-check";
import { Either } from "effect";
import { type Schema, Table } from "../../core/schema.ts";
import type { SelectPlan } from "../../core/plan.ts";
import { compileDDL } from "../../core/compile/ddl.ts";
import { compileSelect } from "../../core/compile/select.ts";
import {
  junkPlan,
  junkSchema,
  schemaAndInvalidPlan,
  schemaAndPlan,
} from "../../tests/property/compiler_arbitraries.ts";

export const FORMAT = "mansio/ir-compile-corpus";

type Case = { schema: Schema; plan: SelectPlan };

const cases: fc.Arbitrary<Case & { family: string }> = fc.oneof(
  { weight: 2, arbitrary: schemaAndPlan.map((c) => ({ ...c, family: "valid" })) },
  {
    weight: 1,
    arbitrary: schemaAndInvalidPlan.map(({ schema, plan }) => ({ schema, plan, family: "defect" })),
  },
  {
    weight: 1,
    arbitrary: junkSchema.chain((schema) =>
      junkPlan(schema).map((plan) => ({ schema, plan, family: "junk" }))
    ),
  },
);

/** The schema JSON as tables, without the declaration checks: the compiler must be total over junk. */
function tablesOf(
  json: Record<string, { fields: Table["fields"]; storage: Table["storage"]; indexes: Table["indexes"] }>,
) {
  return Object.fromEntries(
    Object.entries(json).map(([name, t]) => [name, new Table(t.fields, t.storage, t.indexes)]),
  ) as Schema;
}

function result<A>(either: Either.Either<A, { _tag: string }>, key: string) {
  if (Either.isRight(either)) return { [key]: either.right };
  const { _tag, ...fields } = either.left as unknown as Record<string, unknown>;
  return { error: { tag: _tag, fields: JSON.parse(JSON.stringify(fields)) } };
}

/** One corpus line per case, after the header line. */
export function corpus(count: number, seed: number): string[] {
  const lines = [JSON.stringify({ format: FORMAT, version: 1, seed, count, dialect: "sqlite" })];
  fc.sample(cases, { seed, numRuns: count }).forEach((c, i) => {
    const schemaJson = JSON.parse(JSON.stringify(c.schema));
    const plan = JSON.parse(JSON.stringify(c.plan)) as SelectPlan;
    const schema = tablesOf(schemaJson);
    lines.push(JSON.stringify({
      i,
      family: c.family,
      schema: schemaJson,
      plan,
      select: result(compileSelect(schema, plan, "sqlite"), "sql"),
      ddl: result(compileDDL(schema), "statements"),
    }));
  });
  return lines;
}

if (import.meta.main) {
  const [
    count = "2000",
    seed = "20261105",
    out = "ir/vectors/generated/compile-corpus.jsonl",
  ] = Deno.args;
  const lines = corpus(Number(count), Number(seed));
  await Deno.mkdir(new URL(".", new URL(out, `file://${Deno.cwd()}/`)), { recursive: true });
  await Deno.writeTextFile(out, lines.join("\n") + "\n");
  console.log(`${out}: ${lines.length - 1} cases`);
}
