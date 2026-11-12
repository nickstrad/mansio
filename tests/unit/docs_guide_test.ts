/**
 * `docs/guide.md` against the repo: every `deno task` it names exists, and its H2 outline is fixed. The Deno
 * platform's checks of its environment options went with it at IR-6; the event-log CLI's options are
 * `eventlog/config.ts`'s (`tests/unit/eventlog_config_test.ts`).
 */
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const guide = readFileSync(new URL("../../docs/guide.md", import.meta.url), "utf8");
const config = JSON.parse(readFileSync(new URL("../../deno.json", import.meta.url), "utf8"));
const tasks = new Set<string>(Object.keys(config.tasks));
const headings = [
  "Installation",
  "Configure and start agentd",
  "Core concepts",
  "SDK walkthrough",
  "Authentication and tenancy",
  "Backups and restore",
  "Observability",
  "Troubleshooting",
];

test("guide: every deno task exists", () => {
  const named = [...guide.matchAll(/deno task ([\w:-]+)(\*)?/g)];
  assert.ok(named.length > 0, "expected runnable tasks");
  for (const [, name, star] of named) {
    if (star) {
      // `deno task logs:*`: a family of tasks; at least one must exist.
      assert.ok([...tasks].some((task) => task.startsWith(name)), `No deno task matches ${name}*`);
    } else {
      assert.ok(tasks.has(name), `Unknown deno task: ${name}`);
    }
  }
});

test("guide: the eight H2 headings are present in order, with no extras", () => {
  assert.deepEqual([...guide.matchAll(/^## (.+)$/gm)].map((match) => match[1]), headings);
});
