/**
 * claude-usage.ts — summarize Claude Code CLI token usage from session transcripts.
 *
 * The CLI records per-message token counts in ~/.claude/projects/<project>/*.jsonl.
 * This script sums them per project for a given date (default: today, UTC).
 *
 * Usage:
 *   deno run --allow-read --allow-env work-scripts/deno/claude-usage.ts [--date YYYY-MM-DD]
 *
 * Output: one line per project with input / cache-write / cache-read / output tokens,
 * plus a TOTAL line. Numbers are raw tokens as reported by the API.
 */
import { expandGlob } from "jsr:@std/fs@1.0.24/expand-glob";

interface Usage {
  input_tokens?: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
  output_tokens?: number;
}

interface Totals {
  sessions: Set<string>;
  input: number;
  cacheWrite: number;
  cacheRead: number;
  output: number;
}

function newTotals(): Totals {
  return { sessions: new Set(), input: 0, cacheWrite: 0, cacheRead: 0, output: 0 };
}

function fmt(n: number): string {
  return n.toLocaleString("en-US");
}

let date = new Date().toISOString().slice(0, 10);
const args = Deno.args;
for (let i = 0; i < args.length; i++) {
  if (args[i] === "--date" && args[i + 1]) date = args[i + 1];
}

const home = Deno.env.get("HOME") ?? "/root";
const projectsDir = `${home}/.claude/projects`;
const byProject = new Map<string, Totals>();
const grand = newTotals();

try {
  for await (const entry of expandGlob(`${projectsDir}/*/*.jsonl`)) {
    const project = entry.path.split("/").at(-2) ?? "unknown";
    const t = byProject.get(project) ?? newTotals();
    byProject.set(project, t);
    t.sessions.add(entry.path);
    grand.sessions.add(entry.path);
    const text = await Deno.readTextFile(entry.path);
    for (const line of text.split("\n")) {
      if (!line.trim()) continue;
      let d: {
        type?: string;
        timestamp?: string;
        message?: { usage?: Usage };
      };
      try {
        d = JSON.parse(line);
      } catch {
        continue;
      }
      if (d.type !== "assistant") continue;
      if (!d.timestamp?.startsWith(date)) continue;
      const u = d.message?.usage;
      if (!u) continue;
      const inp = u.input_tokens ?? 0;
      const cw = u.cache_creation_input_tokens ?? 0;
      const cr = u.cache_read_input_tokens ?? 0;
      const out = u.output_tokens ?? 0;
      t.input += inp;
      t.cacheWrite += cw;
      t.cacheRead += cr;
      t.output += out;
      grand.input += inp;
      grand.cacheWrite += cw;
      grand.cacheRead += cr;
      grand.output += out;
    }
  }
} catch (e) {
  console.error(`cannot read ${projectsDir}: ${e}`);
  Deno.exit(1);
}

const rows = [...byProject.entries()]
  .filter(([, t]) => t.input + t.cacheWrite + t.cacheRead + t.output > 0)
  .sort((a, b) =>
    (b[1].input + b[1].cacheWrite + b[1].cacheRead + b[1].output) -
    (a[1].input + a[1].cacheWrite + a[1].cacheRead + a[1].output)
  );

console.log(`claude usage for ${date} (UTC) — tokens`);
const header = "project".padEnd(34) + "  sessions" + "      input" + "   cache-write" +
  "    cache-read" + "     output" + "      total";
console.log(header);
for (const [project, t] of rows) {
  const total = t.input + t.cacheWrite + t.cacheRead + t.output;
  const name = project.replace(/^-/, "").replace(/-/g, "/").slice(0, 32);
  console.log(
    name.padEnd(34) +
      String(t.sessions.size).padStart(9) +
      fmt(t.input).padStart(11) +
      fmt(t.cacheWrite).padStart(14) +
      fmt(t.cacheRead).padStart(13) +
      fmt(t.output).padStart(11) +
      fmt(total).padStart(11),
  );
}
const gTotal = grand.input + grand.cacheWrite + grand.cacheRead + grand.output;
console.log(
  "TOTAL".padEnd(34) +
    String(grand.sessions.size).padStart(9) +
    fmt(grand.input).padStart(11) +
    fmt(grand.cacheWrite).padStart(14) +
    fmt(grand.cacheRead).padStart(13) +
    fmt(grand.output).padStart(11) +
    fmt(gTotal).padStart(11),
);
