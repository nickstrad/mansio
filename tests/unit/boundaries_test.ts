/**
 * Boundary tests (plan item 26): the architectural layering rules, asserted by scanning source
 * text so the boundaries cannot rot silently.
 *
 * Rules:
 * - no file in `core/` imports `sdk/`, `node:*` or `npm:*`, or references `Deno.`;
 * - no `console.*` in the shipped library;
 * - no `Deno.env` in the shipped library;
 * - no `throw new Error` in `core/` (errors go through the tagged channel).
 *
 * Until IR-6 these rules covered the Deno platform too, and a second group kept `node:sqlite`
 * out of the processes that opened files through the FFI SQLite binding (one SQLite copy per process).
 * The platform and the binding are gone, so that group went with them. The offline event-log tool
 * (`eventlog/`, ADR 0014) had its own exceptions to the `console.*` and `Deno.env` rules; it is gone too.
 *
 * Scoping notes, deliberate:
 * - The `console.*` and `Deno.env` rules cover the shipped library (`core/`, `sdk/`,
 *   `examples/tic-tac-toe/`). `tests/` is scaffolding (coverage reports, skip notices), `scripts/` are CLIs, and
 *   `agent-work/work-scripts/`/`perf/` are operator tooling — all legitimately use `console` and env.
 * - Comments are stripped before scanning, so prose about a boundary is not flagged; string literals
 *   are kept, so import specifiers are.
 */
import assert from "node:assert/strict";
import { dirname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = fileURLToPath(new URL("../..", import.meta.url));

/** All files under `dir` whose name passes `keep`, recursively, as repo-relative paths. */
async function filesUnder(dir: string, keep: (name: string) => boolean): Promise<string[]> {
  const out: string[] = [];
  for await (const entry of Deno.readDir(join(ROOT, dir))) {
    const rel = join(dir, entry.name);
    if (entry.isDirectory) out.push(...await filesUnder(rel, keep));
    else if (entry.isFile && keep(entry.name)) out.push(rel);
  }
  return out.sort();
}

/** All `.ts` files under `dir`, recursively, as repo-relative paths. */
function tsFiles(dir: string): Promise<string[]> {
  return filesUnder(dir, (name) => name.endsWith(".ts"));
}

const sources = new Map<string, string>();
async function load(rel: string): Promise<string> {
  let src = sources.get(rel);
  if (src === undefined) {
    src = await Deno.readTextFile(join(ROOT, rel));
    sources.set(rel, src);
  }
  return src;
}

/**
 * Remove `//` and `/* ... *\/` comments, keeping code and string literals (so import
 * specifiers stay visible to the import rules).
 */
function noComments(src: string): string {
  let out = "";
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (c === "/" && src[i + 1] === "/") {
      const nl = src.indexOf("\n", i + 2);
      out += "\n";
      i = nl === -1 ? src.length : nl + 1;
      continue;
    }
    if (c === "/" && src[i + 1] === "*") {
      const end = src.indexOf("*/", i + 2);
      i = end === -1 ? src.length : end + 2;
      continue;
    }
    if (c === '"' || c === "'" || c === "`") {
      const quote = c;
      out += c;
      i++;
      while (i < src.length) {
        if (src[i] === "\\") {
          out += src.slice(i, i + 2);
          i += 2;
          continue;
        }
        out += src[i];
        if (src[i] === quote) {
          i++;
          break;
        }
        i++;
      }
      continue;
    }
    out += c;
    i++;
  }
  return out;
}

/** Static and dynamic import specifiers in comment-free code. */
function importSpecifiers(code: string): string[] {
  const specs: string[] = [];
  const re =
    /(?:import|export)\s+(?:[^"'`;]*?\sfrom\s+)?["']([^"']+)["']|import\s*\(\s*["']([^"']+)["']\s*\)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(code)) !== null) specs.push(m[1] ?? m[2]);
  return specs;
}

/** The first line matching `re`, trimmed, or undefined. */
function firstMatchLine(code: string, re: RegExp): string | undefined {
  for (const line of code.split("\n")) {
    if (re.test(line)) return line.trim().slice(0, 120);
  }
  return undefined;
}

async function violations(
  rels: string[],
  check: (rel: string, code: string) => string | undefined,
): Promise<string[]> {
  const found: string[] = [];
  for (const rel of rels) {
    const hit = check(rel, noComments(await load(rel)));
    if (hit !== undefined) found.push(`${rel}: ${hit}`);
  }
  return found;
}

/** The shipped library the `console.*`/`Deno.env` rules guard. */
const LIB = ["core", "sdk", "examples"];

Deno.test("boundaries: core/ imports nothing from sdk/, node:* or npm:* and never references Deno.", async () => {
  const bad = await violations(await tsFiles("core"), (rel, code) => {
    for (const spec of importSpecifiers(code)) {
      const target = spec.startsWith(".") ? normalize(join(dirname(rel), spec)) : spec;
      if (target.startsWith("sdk/") || spec.startsWith("node:") || spec.startsWith("npm:")) {
        return `forbidden import ${JSON.stringify(spec)}`;
      }
    }
    const line = firstMatchLine(code, /\bDeno\./);
    return line === undefined ? undefined : `Deno.* reference: ${line}`;
  });
  assert.deepEqual(bad, []);
});

Deno.test("boundaries: no console.* in the shipped library", async () => {
  const rels: string[] = [];
  for (const dir of LIB) rels.push(...await tsFiles(dir));
  const bad = await violations(rels, (_rel, code) => {
    const line = firstMatchLine(code, /\bconsole\.\w+/);
    return line === undefined ? undefined : `console usage: ${line}`;
  });
  assert.deepEqual(bad, []);
});

Deno.test("boundaries: no Deno.env in the shipped library", async () => {
  const rels: string[] = [];
  for (const dir of LIB) rels.push(...await tsFiles(dir));
  const bad = await violations(rels, (_rel, code) => {
    const line = firstMatchLine(code, /\bDeno\.env\b/);
    return line === undefined ? undefined : `Deno.env usage: ${line}`;
  });
  assert.deepEqual(bad, []);
});

Deno.test("boundaries: no `throw new Error` in core/ — errors go through the tagged channel", async () => {
  const bad = await violations(await tsFiles("core"), (_rel, code) => {
    const line = firstMatchLine(code, /\bthrow\s+new\s+Error\b/);
    return line === undefined ? undefined : `untyped throw: ${line}`;
  });
  assert.deepEqual(bad, []);
});
