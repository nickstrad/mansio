/**
 * Drift guard for the Go copy of `scripts/explain.ts`'s inputs (IR-1, `server/internal/doccompile`
 * `TestGeneratedSQL`). The Go test regenerates `core/compile/GENERATED_SQL.txt` from frozen JSON in
 * `server/internal/doccompile/testdata/`; these tests fail on the TS side when that JSON, or the committed
 * `core/compile/GENERATED_SQL.txt`, no longer matches what the app schema and the SDK produce.
 */
import assert from "node:assert/strict";
import { Effect } from "effect";
import { schema } from "../../examples/tic-tac-toe/schema.ts";
import { Documents, type SelectPlan } from "../../sdk/db.ts";

const ROOT = new URL("../../", import.meta.url);
const TESTDATA = new URL("server/internal/doccompile/testdata/", ROOT);

async function json(name: string): Promise<unknown> {
  return JSON.parse(await Deno.readTextFile(new URL(name, TESTDATA)));
}

Deno.test("go testdata: app_schema.json is JSON.stringify of examples/tic-tac-toe/schema.ts", async () => {
  const text = (await Deno.readTextFile(new URL("app_schema.json", TESTDATA))).trimEnd();
  assert.equal(
    text,
    JSON.stringify(schema),
    "regenerate server/internal/doccompile/testdata/app_schema.json",
  );
});

Deno.test("go testdata: explain_plan.json is scripts/explain.ts's plan", async () => {
  let plan: SelectPlan | undefined;
  const db = new Documents<typeof schema>((op) =>
    Effect.sync(() => {
      plan = op as SelectPlan;
      return [];
    })
  );
  await Effect.runPromise(
    db.find("games", { status: "playing" }).sort("_creationTime", "desc").limit(20).collect(),
  );
  // Member order matters too: the Go test pretty-prints this JSON into core/compile/GENERATED_SQL.txt.
  assert.equal(JSON.stringify(await json("explain_plan.json")), JSON.stringify(plan));
});

Deno.test("core/compile/GENERATED_SQL.txt is current (deno task explain)", async () => {
  const out = await new Deno.Command(Deno.execPath(), {
    args: ["run", "scripts/explain.ts"],
    cwd: ROOT,
    stdout: "piped",
    stderr: "inherit",
  }).output();
  assert.ok(out.success, "scripts/explain.ts failed");
  assert.equal(
    new TextDecoder().decode(out.stdout),
    await Deno.readTextFile(new URL("core/compile/GENERATED_SQL.txt", ROOT)),
    "core/compile/GENERATED_SQL.txt is stale: run deno task explain > core/compile/GENERATED_SQL.txt",
  );
});
