/**
 * Unit tests for the retention sweeper (plan items 24 and 36), run directly against
 * `platform/logging/retention.ts` — not through the `logs:sweep` CLI, which needs item 23's
 * `scripts/logs.ts`.
 *
 * Since item 36 a sweep needs an ingest ledger. These tests are about the file contract, so most of
 * them use `diskLedger`, which vouches that everything on disk is fully ingested; the gate itself,
 * against a real ingest, is `tests/unit/retention_ingest_test.ts`. The cases here that are about the
 * gate use hand-built ledgers that stop short.
 *
 * Every test builds a synthetic `LOG_DIR` in a fresh temp directory and asserts on what is actually
 * left on disk, because the contract is a file contract: exactly these files gzipped, exactly these
 * deleted, everything else (today above all) byte-for-byte untouched.
 *
 * "Now" is `NOW` (2026-10-11 12:00 local), supplied through `TestClock`; dated directories are named
 * through `rotation.currentPath` so the fixture and the sweeper cannot disagree about the layout.
 * The expected outcome of the 30-day tree is computed here from the policy numbers, not by calling
 * `rotation.plan`, so the test is an independent statement of what the policy means.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { dirname, join, relative } from "node:path";
import { gunzipSync, gzipSync } from "node:zlib";
import { Cause, Duration, Effect, Exit, Fiber, Option, TestClock } from "effect";
import {
  type CommittedWatermark,
  currentPath,
  plan as rotationPlan,
  recordOffsets,
  type RotationPolicy,
} from "../../core/logging/rotation.ts";
import type { EventRecord } from "../../core/events.ts";
import { ConfigTest } from "../../platform/config.ts";
import {
  applyPlan,
  applySafetyRails,
  type IngestLedger,
  inventory,
  RetentionPathError,
  startRetentionSweeper,
  sweep,
  type SweepOptions,
  syncDir,
  validatePlan,
  type VerifiedSource,
} from "../../platform/logging/retention.ts";
import { events } from "../layers/index.ts";
import { runEffect } from "../helpers/run.ts";

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const NOW = new Date(2026, 9, 11, 12, 0, 0);
const TODAY = "2026-10-11";
const POLICY: RotationPolicy = {
  keepDays: 14,
  gzipAfterDays: 2,
  maxTotalBytes: Number.MAX_SAFE_INTEGER,
  maxFileBytes: 64 * 1024 * 1024,
};
const PHASE = "retention-sweep";

function daysAgo(age: number, hour = 12): Date {
  return new Date(2026, 9, 11 - age, hour, 0, 0);
}

/** Where `rotation.currentPath` puts a file for `age` days ago, re-rooted under `root`. */
function eventPath(
  root: string,
  role: "server" | "reader" | "restore" | "cli",
  age: number,
  hour: number,
  pid: number,
  seq: number,
): string {
  return join(root, currentPath(POLICY, daysAgo(age, hour), role, pid, seq).slice("logs/".length));
}

interface Fixture {
  readonly path: string;
  readonly age: number;
  readonly content: string;
}

async function put(path: string, content: string | Uint8Array): Promise<void> {
  await Deno.mkdir(dirname(path), { recursive: true });
  await Deno.writeFile(path, typeof content === "string" ? new TextEncoder().encode(content) : content);
}

/** `put`, with the mtime a file opened `age` days ago at `hour` would have: settled, as old files are. */
async function putAged(path: string, content: string | Uint8Array, age: number, hour = 12): Promise<void> {
  await put(path, content);
  const stamp = daysAgo(age, hour);
  await Deno.utime(path, stamp, stamp);
}

const sha256 = (bytes: Uint8Array) => createHash("sha256").update(bytes).digest("hex");

/** A file's content as ingest saw it: decompressed for a `.gz`. */
async function contentOf(path: string): Promise<Uint8Array> {
  const raw = await Deno.readFile(path);
  return path.endsWith(".gz") ? new Uint8Array(gunzipSync(raw)) : raw;
}

/**
 * A ledger that vouches every event file under `root` is fully ingested, every record accepted —
 * read from disk when the sweep asks, so files that appear later are covered too. For a source
 * present both plain and as `.gz`, the plain file is what was ingested.
 */
function diskLedger(root: string): Effect.Effect<IngestLedger> {
  return Effect.promise(async () => {
    const watermarks = new Map<string, CommittedWatermark>();
    const accepted = new Map<string, ReadonlySet<number>>();
    const files = (await listFiles(root)).filter((p) => /\/events-[^/]+\.jsonl(\.gz)?$/.test(p)).sort();
    for (const file of files) {
      const source = file.replace(/\.gz$/, "");
      if (watermarks.has(source)) continue;
      const content = await contentOf(join(root, file));
      watermarks.set(source, { bytesIngested: content.length, prefixSha256: sha256(content), dedupe: false });
      accepted.set(source, new Set(recordOffsets(content)));
    }
    return {
      watermarks,
      acceptedOffsets: (sources: readonly string[]) =>
        Effect.succeed(new Map(sources.map((s) => [s, accepted.get(s) ?? new Set<number>()]))),
    };
  });
}

/** What the gate would have proved about `paths`, for tests that call `applyPlan` directly. */
async function verifiedFor(paths: readonly string[]): Promise<Map<string, VerifiedSource>> {
  const out = new Map<string, VerifiedSource>();
  for (const path of paths) {
    const content = await contentOf(path);
    out.set(path, { sha256: sha256(content), bytes: content.length });
  }
  return out;
}

/**
 * Three event files per day for 30 days (today = age 0): two from the server, one from the reader.
 * Plus a pre-compressed file at age 5 and age 20, and files outside any role tree that a sweep must
 * never look at.
 */
async function writeTree(root: string): Promise<Fixture[]> {
  const fixtures: Fixture[] = [];
  const add = async (path: string, age: number, content: string, hour: number) => {
    await putAged(path, content, age, hour);
    fixtures.push({ path, age, content });
  };
  for (let age = 0; age < 30; age++) {
    for (
      const [role, hour, pid] of [["server", 9, 100], ["server", 15, 100], ["reader", 9, 200]] as const
    ) {
      await add(
        eventPath(root, role, age, hour, pid, 1),
        age,
        `${JSON.stringify({ age, role, hour })}\n`.repeat(20),
        hour,
      );
    }
  }
  for (const age of [5, 20]) {
    const path = `${eventPath(root, "server", age, 3, 100, 1)}.gz`;
    await putAged(path, gzipSync("already compressed\n"), age, 3);
    fixtures.push({ path, age, content: "already compressed\n" });
  }
  await put(join(root, "k6", "run-1", "raw.json"), "{}");
  await put(join(root, "duckdb", "events.duckdb"), "not really duckdb");
  await put(join(root, "server", "notes.txt"), "operator notes");
  return fixtures;
}

/** Every file and symlink under `root`, as sorted `/`-separated relative paths. */
async function listFiles(root: string): Promise<string[]> {
  const found: string[] = [];
  const walk = async (dir: string): Promise<void> => {
    for await (const entry of Deno.readDir(dir)) {
      const full = join(dir, entry.name);
      if (entry.isDirectory && !entry.isSymlink) await walk(full);
      else found.push(relative(root, full));
    }
  };
  await walk(root);
  return found.sort();
}

/** Every directory under `root`, relative and sorted. */
async function listDirs(root: string): Promise<string[]> {
  const found: string[] = [];
  const walk = async (dir: string): Promise<void> => {
    for await (const entry of Deno.readDir(dir)) {
      if (!entry.isDirectory || entry.isSymlink) continue;
      const full = join(dir, entry.name);
      found.push(relative(root, full));
      await walk(full);
    }
  };
  await walk(root);
  return found.sort();
}

/** Type, size, mtime and content hash of everything under `root`, directories included. */
async function snapshot(root: string): Promise<string[]> {
  const rows: string[] = [];
  const walk = async (dir: string): Promise<void> => {
    for await (const entry of Deno.readDir(dir)) {
      const full = join(dir, entry.name);
      const info = await Deno.lstat(full);
      const rel = relative(root, full);
      if (info.isDirectory) {
        rows.push(`d ${rel} ${info.mtime?.getTime()}`);
        await walk(full);
      } else if (info.isSymlink) {
        rows.push(`l ${rel} -> ${await Deno.readLink(full)}`);
      } else {
        const hash = createHash("sha256").update(await Deno.readFile(full)).digest("hex");
        rows.push(`f ${rel} ${info.size} ${info.mtime?.getTime()} ${hash}`);
      }
    }
  };
  await walk(root);
  return rows.sort();
}

async function withTempDir(run: (dir: string) => Promise<void>): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "mansio-retention-" });
  try {
    await run(dir);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
}

const rel = (root: string, path: string) => relative(root, path);

/** The file the 30-day tree should hold after one sweep under `POLICY`. */
function expectedAfterSweep(root: string, fixtures: readonly Fixture[]): string[] {
  const after: string[] = [];
  for (const { path, age } of fixtures) {
    if (age >= POLICY.keepDays) continue;
    const compress = age >= POLICY.gzipAfterDays && !path.endsWith(".gz");
    after.push(rel(root, compress ? `${path}.gz` : path));
  }
  after.push("k6/run-1/raw.json", "duckdb/events.duckdb", "server/notes.txt");
  return after.sort();
}

function lifecycleEvents(all: readonly EventRecord[]) {
  return all.flatMap((e) => (e.kind === "lifecycle" ? [e] : []));
}

/** Runs `sweep` under the test stack with the clock at `NOW`; returns the exit and every event. */
function runSweep(options: SweepOptions) {
  return runEffect(Effect.gen(function* () {
    yield* TestClock.setTime(NOW.getTime());
    const exit = yield* Effect.exit(sweep(options));
    return { exit, emitted: yield* events };
  }));
}

function sweepOptions(root: string, overrides: Partial<SweepOptions> = {}): SweepOptions {
  return { logDir: root, policy: POLICY, role: "cli" as const, ledger: diskLedger(root), ...overrides };
}

function succeeded<A, E>(exit: Exit.Exit<A, E>): A {
  if (Exit.isSuccess(exit)) return exit.value;
  throw new Error(Cause.pretty(exit.cause));
}

function failureOf<A, E>(exit: Exit.Exit<A, E>): E {
  assert.ok(Exit.isFailure(exit), "expected the sweep to fail");
  const failure = Cause.failureOption(exit.cause);
  assert.ok(Option.isSome(failure), `expected a typed failure, got ${Cause.pretty(exit.cause)}`);
  return failure.value;
}

// ---------------------------------------------------------------------------
// The 30-day tree
// ---------------------------------------------------------------------------

test("30-day tree: gzips days 2–13, deletes day 14+, keeps today and yesterday untouched", async () => {
  await withTempDir(async (root) => {
    const fixtures = await writeTree(root);
    const recent = fixtures.filter((f) => f.age <= 1);
    const recentBefore = await Promise.all(recent.map(async (f) => ({
      path: f.path,
      content: f.content,
      mtime: (await Deno.stat(f.path)).mtime?.getTime(),
    })));

    const { exit, emitted } = await runSweep(sweepOptions(root));
    const report = succeeded(exit);

    assert.deepEqual(await listFiles(root), expectedAfterSweep(root, fixtures));

    const expectGzipped = fixtures
      .filter((f) => f.age >= 2 && f.age < 14 && !f.path.endsWith(".gz"))
      .map((f) => rel(root, f.path)).sort();
    const expectDeleted = fixtures.filter((f) => f.age >= 14).map((f) => rel(root, f.path)).sort();
    assert.deepEqual([...report.gzipped].sort(), expectGzipped);
    assert.deepEqual([...report.deleted].sort(), expectDeleted);
    assert.equal(expectGzipped.length, 12 * 3);
    assert.equal(expectDeleted.length, 16 * 3 + 1, "16 days of three files, plus the age-20 .gz");
    assert.deepEqual(report.skipped, []);

    // Every dated directory 14+ days old is gone, in both roles, and nothing else was removed.
    const dirs = await listDirs(root);
    for (let age = 14; age < 30; age++) {
      for (const role of ["server", "reader"] as const) {
        assert.ok(!dirs.includes(rel(root, dirname(eventPath(root, role, age, 9, 100, 1)))));
      }
    }
    assert.equal(report.removedDirs.length, 16 * 2);
    assert.ok(dirs.includes("server/2026-10-11") && dirs.includes("reader/2026-10-10"));

    // Today's and yesterday's files: same bytes, same mtime.
    for (const before of recentBefore) {
      assert.equal(await Deno.readTextFile(before.path), before.content);
      assert.equal((await Deno.stat(before.path)).mtime?.getTime(), before.mtime);
    }

    // The compressed copies decompress to the original bytes and keep the original's age.
    for (const f of fixtures.filter((x) => x.age >= 2 && x.age < 14 && !x.path.endsWith(".gz"))) {
      assert.equal(gunzipSync(await Deno.readFile(`${f.path}.gz`)).toString("utf8"), f.content);
    }

    // One lifecycle event, naming exactly what happened.
    const lifecycle = lifecycleEvents(emitted);
    assert.equal(lifecycle.length, 1);
    assert.equal(lifecycle[0].phase, PHASE);
    assert.equal(lifecycle[0].level, "info");
    assert.equal(lifecycle[0].role, "cli");
    const detail = lifecycle[0].detail as Record<string, unknown>;
    assert.deepEqual(detail.gzipped, report.gzipped);
    assert.deepEqual(detail.deleted, report.deleted);
    assert.deepEqual(detail.removedDirs, report.removedDirs);
    assert.deepEqual(detail.futureDated, []);
    // Counts ride along with the path lists: gzipped, deleted, and everything else left as found.
    assert.equal(detail.gzippedCount, expectGzipped.length);
    assert.equal(detail.deletedCount, expectDeleted.length);
    assert.equal(detail.keptCount, fixtures.length - expectGzipped.length - expectDeleted.length);
    assert.deepEqual(report.futureDated, []);
  });
});

test("a second sweep of the same tree changes nothing", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    succeeded((await runSweep(sweepOptions(root))).exit);
    const before = await snapshot(root);

    const second = succeeded((await runSweep(sweepOptions(root))).exit);

    assert.deepEqual(await snapshot(root), before);
    assert.deepEqual(second.gzipped, []);
    assert.deepEqual(second.deleted, []);
    assert.deepEqual(second.removedDirs, []);
  });
});

test("the sweep does exactly what rotation.plan says, including the total-bytes cap", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const policy = { ...POLICY, maxTotalBytes: 6_000 };
    const inv = await Effect.runPromise(inventory(root));
    const expected = rotationPlan(inv.entries, NOW, policy);
    assert.ok(
      expected.delete.some((p) => /\/2026-10-0\d\//.test(p)),
      "the cap must reach below keepDays for this test to say anything",
    );

    succeeded((await runSweep(sweepOptions(root, { policy }))).exit);

    for (const path of expected.delete) assert.equal(await exists(path), false, path);
    for (const path of expected.gzip) {
      assert.equal(await exists(path), false, path);
      assert.equal(await exists(`${path}.gz`), true, `${path}.gz`);
    }
    for (const path of expected.keep) assert.equal(await exists(path), true, path);
  });
});

async function exists(path: string): Promise<boolean> {
  try {
    await Deno.lstat(path);
    return true;
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return false;
    throw cause;
  }
}

// ---------------------------------------------------------------------------
// Dry run
// ---------------------------------------------------------------------------

test("dry run reports the plan and changes nothing on disk, not even directories", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const before = await snapshot(root);

    const { exit, emitted } = await runSweep(sweepOptions(root, { dryRun: true }));
    const report = succeeded(exit);

    assert.deepEqual(await snapshot(root), before);
    assert.equal(report.dryRun, true);
    assert.equal(report.plan.gzip.length, 12 * 3);
    assert.equal(report.plan.delete.length, 16 * 3 + 1);
    assert.equal(report.emptiedDirs.length, 16 * 2);
    assert.deepEqual(report.gzipped, []);
    assert.deepEqual(report.deleted, []);
    assert.deepEqual(report.removedDirs, []);
    assert.deepEqual(emitted, [], "an event sink writes a file; a dry run must not emit");
  });
});

test("a dry run that fails still emits its error event, with counts, and changes nothing", async () => {
  await withTempDir(async (base) => {
    const root = join(base, "logs");
    await writeTree(root);
    const inv = await Effect.runPromise(inventory(root));
    const before = await snapshot(base);

    const { exit, emitted } = await runEffect(Effect.gen(function* () {
      yield* TestClock.setTime(NOW.getTime());
      const exit = yield* Effect.exit(applyPlan({
        root,
        inventory: inv,
        plan: { gzip: [], delete: ["/etc/passwd"], keep: [] },
        verified: new Map(),
        today: TODAY,
        role: "cli",
        dryRun: true,
      }));
      return { exit, emitted: yield* events };
    }));

    assert.equal(failureOf(exit) instanceof RetentionPathError, true);
    const errors = lifecycleEvents(emitted);
    assert.equal(errors.length, 1);
    assert.equal(errors[0].level, "error");
    assert.equal(errors[0].phase, `${PHASE}-failed`);
    const detail = errors[0].detail as Record<string, unknown>;
    assert.deepEqual(
      [detail.gzippedCount, detail.deletedCount, detail.keptCount],
      [0, 0, inv.entries.length],
    );
    assert.deepEqual(await snapshot(base), before);

    // And a dry run refused before it ever reads the tree.
    const unsafe = await runSweep(sweepOptions("/", { dryRun: true }));
    assert.equal(failureOf(unsafe.exit) instanceof RetentionPathError, true);
    assert.equal(lifecycleEvents(unsafe.emitted).length, 1);
  });
});

// ---------------------------------------------------------------------------
// Safety rails
// ---------------------------------------------------------------------------

test("a file a sink holds open is neither gzipped nor deleted, and keeps its directory", async () => {
  await withTempDir(async (root) => {
    const fixtures = await writeTree(root);
    const openOld = eventPath(root, "server", 20, 9, 100, 1);
    const openMid = eventPath(root, "server", 5, 9, 100, 1);

    const { exit } = await runSweep(
      sweepOptions(root, { openPaths: Effect.succeed([openOld, openMid]) }),
    );
    const report = succeeded(exit);

    assert.equal(await exists(openOld), true);
    assert.equal(await exists(openMid), true);
    assert.equal(await exists(`${openMid}.gz`), false);
    assert.equal(await Deno.readTextFile(openMid), fixtures.find((f) => f.path === openMid)!.content);
    assert.ok(report.removedDirs.every((d) => d !== "server/2026-09-21"));
    assert.ok(report.removedDirs.includes("reader/2026-09-21"));
  });
});

test("anything else in a dated directory is left alone and keeps the directory", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const oldDir = dirname(eventPath(root, "server", 20, 9, 100, 1));
    await put(join(oldDir, "README.txt"), "keep me");
    await put(join(oldDir, "subdir", "x"), "keep me too");

    const report = succeeded((await runSweep(sweepOptions(root))).exit);

    assert.equal(await Deno.readTextFile(join(oldDir, "README.txt")), "keep me");
    assert.equal(await exists(join(oldDir, "subdir", "x")), true);
    assert.deepEqual(await listFiles(oldDir), ["README.txt", "subdir/x"]);
    assert.ok(!report.removedDirs.includes(rel(root, oldDir)));
    assert.deepEqual(report.skipped, []);
  });
});

test("rejected.jsonl ages out with its directory but is never gzipped", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const midDir = dirname(eventPath(root, "server", 5, 9, 100, 1));
    const oldDir = dirname(eventPath(root, "server", 20, 9, 100, 1));
    await put(join(midDir, "rejected.jsonl"), "bad line\n");
    await put(join(oldDir, "rejected.jsonl"), "bad line\n");

    const report = succeeded((await runSweep(sweepOptions(root))).exit);

    assert.equal(await Deno.readTextFile(join(midDir, "rejected.jsonl")), "bad line\n");
    assert.equal(await exists(join(midDir, "rejected.jsonl.gz")), false);
    assert.equal(await exists(oldDir), false, "the aged-out directory goes, quarantine file and all");
    assert.ok(report.deleted.includes("server/2026-09-21/rejected.jsonl"));
  });
});

test("a dated directory that is a symlink is not followed", async () => {
  await withTempDir(async (base) => {
    const root = join(base, "logs");
    const outside = join(base, "outside");
    await writeTree(root);
    const victim = join(outside, "events-09-100-001.jsonl");
    await put(victim, "precious\n");
    await Deno.symlink(outside, join(root, "server", "2026-09-01"));

    succeeded((await runSweep(sweepOptions(root))).exit);

    assert.equal(await Deno.readTextFile(victim), "precious\n");
    assert.equal(await exists(`${victim}.gz`), false);
    assert.equal((await Deno.lstat(join(root, "server", "2026-09-01"))).isSymlink, true);
  });
});

test("a planned path outside LOG_DIR refuses the whole plan and emits an error event", async () => {
  await withTempDir(async (base) => {
    const root = join(base, "logs");
    await writeTree(root);
    const sibling = join(base, "logs-evil", "server", "2026-09-01", "events-09-100-001.jsonl");
    await put(sibling, "not yours\n");
    const inv = await Effect.runPromise(inventory(root));
    const legit = eventPath(root, "server", 20, 9, 100, 1);
    const verified = await verifiedFor([legit]);
    const before = await snapshot(base);

    const crafted: Array<[string, string]> = [
      ["outside-root", "/etc/passwd"],
      ["outside-root", join(root, "..", "logs-evil", "server", "2026-09-01", "events-09-100-001.jsonl")],
      ["outside-root", sibling], // shares LOG_DIR as a string prefix, but is not under it
      ["not-absolute", "server/2026-09-01/events-09-100-001.jsonl"],
      ["unexpected-shape", join(root, "duckdb", "events.duckdb")],
      ["unexpected-shape", join(root, "server", "notes.txt")],
      ["not-inventoried", eventPath(root, "server", 25, 4, 7, 1)],
      ["today-or-future", eventPath(root, "server", 0, 9, 100, 1)],
      // Inventoried, well shaped, old enough — but the gate never passed it.
      ["unverified", eventPath(root, "server", 21, 9, 100, 1)],
    ];
    for (const [reason, path] of crafted) {
      const { exit, emitted } = await runEffect(Effect.gen(function* () {
        yield* TestClock.setTime(NOW.getTime());
        // The crafted path rides along with a perfectly legitimate delete: all or nothing.
        const exit = yield* Effect.exit(applyPlan({
          root,
          inventory: inv,
          plan: { gzip: [], delete: [legit, path], keep: [] },
          verified,
          today: TODAY,
          role: "cli",
        }));
        return { exit, emitted: yield* events };
      }));

      const error = failureOf(exit);
      assert.ok(error instanceof RetentionPathError, `${path}: ${String(error)}`);
      assert.equal(error.reason, reason, path);
      const errors = lifecycleEvents(emitted).filter((e) => e.level === "error");
      assert.equal(errors.length, 1, path);
      assert.equal((errors[0].detail as Record<string, unknown>).errorTag, "RetentionPathError");
      assert.equal((errors[0].detail as Record<string, unknown>).reason, reason);
      assert.equal((errors[0].detail as Record<string, unknown>).gzippedCount, 0);
      assert.equal((errors[0].detail as Record<string, unknown>).deletedCount, 0);
      assert.equal((errors[0].detail as Record<string, unknown>).keptCount, inv.entries.length);
      assert.deepEqual(await snapshot(base), before, `nothing may change for ${path}`);
    }
    assert.equal(await exists(legit), true);
  });
});

test("a file reached through a symlinked parent directory is refused", async () => {
  await withTempDir(async (base) => {
    const root = join(base, "logs");
    const outside = join(base, "outside");
    await put(join(outside, "events-09-100-001.jsonl"), "precious\n");
    await Deno.mkdir(join(root, "server"), { recursive: true });
    await Deno.symlink(outside, join(root, "server", "2026-09-01"));
    const path = join(root, "server", "2026-09-01", "events-09-100-001.jsonl");

    const exit = await Effect.runPromiseExit(
      validatePlan(root, { gzip: [], delete: [path] }, new Set([path]), TODAY),
    );

    const error = failureOf(exit);
    assert.ok(error instanceof RetentionPathError);
    assert.equal(error.reason, "symlink-escape");
  });
});

test("a `link/..` path acts on the resolved path only, never on what the OS would reach", async () => {
  await withTempDir(async (base) => {
    const root = join(base, "logs");
    const outside = join(base, "outside");
    const deleteName = "events-09-100-001.jsonl";
    const gzipName = "events-10-100-001.jsonl";
    await Deno.mkdir(outside);
    await Deno.mkdir(join(root, "server"), { recursive: true });
    // `link` points at `base/outside`, so the OS reads `server/link/..` as `base`, while a lexical
    // `resolve` reads it as `server` — the two disagree about which `2026-09-01` is meant.
    await Deno.symlink(outside, join(root, "server", "link"));

    // The in-root files and the look-alikes outside have the same bytes and the same mtime, so
    // the identity recheck cannot be what saves the outside ones: only acting on the resolved path can.
    const stamp = new Date(2026, 8, 1, 9, 0, 0);
    const mine = [
      join(root, "server", "2026-09-01", deleteName),
      join(root, "server", "2026-09-02", gzipName),
    ];
    const victims = [join(base, "2026-09-01", deleteName), join(base, "2026-09-02", gzipName)];
    for (const path of [...mine, ...victims]) {
      await put(path, "precious\n");
      await Deno.utime(path, stamp, stamp);
    }
    const victimsBefore = await snapshot(join(base, "2026-09-01"));
    const victimsBefore2 = await snapshot(join(base, "2026-09-02"));
    const inv = await Effect.runPromise(inventory(root));
    const verified = await verifiedFor(mine);
    // `join` would collapse the `..`; the whole point is that the string keeps it.
    const rawDelete = `${root}/server/link/../2026-09-01/${deleteName}`;
    const rawGzip = `${root}/server/link/../2026-09-02/${gzipName}`;

    const { exit, emitted } = await runEffect(Effect.gen(function* () {
      yield* TestClock.setTime(NOW.getTime());
      const exit = yield* Effect.exit(applyPlan({
        root,
        inventory: inv,
        plan: { gzip: [rawGzip], delete: [rawDelete], keep: [] },
        verified,
        today: TODAY,
        role: "cli",
      }));
      return { exit, emitted: yield* events };
    }));
    const report = succeeded(exit);

    // (a) Nothing outside LOG_DIR was touched.
    assert.deepEqual(await snapshot(join(base, "2026-09-01")), victimsBefore);
    assert.deepEqual(await snapshot(join(base, "2026-09-02")), victimsBefore2);
    assert.equal(await Deno.readTextFile(victims[0]), "precious\n");
    assert.equal(await exists(`${victims[1]}.gz`), false);
    assert.deepEqual(await listFiles(outside), []);
    // The files the validated (resolved) paths name are the ones that were acted on.
    assert.equal(await exists(mine[0]), false);
    assert.equal(await exists(mine[1]), false);
    assert.equal(gunzipSync(await Deno.readFile(`${mine[1]}.gz`)).toString("utf8"), "precious\n");

    // (b) The report and the event record the resolved path, not the `link/..` spelling.
    const resolvedDelete = `server/2026-09-01/${deleteName}`;
    const resolvedGzip = `server/2026-09-02/${gzipName}`;
    assert.deepEqual(report.deleted, [resolvedDelete]);
    assert.deepEqual(report.gzipped, [resolvedGzip]);
    assert.deepEqual(report.skipped, []);
    const lifecycle = lifecycleEvents(emitted);
    assert.equal(lifecycle.length, 1);
    const detail = lifecycle[0].detail as Record<string, unknown>;
    assert.deepEqual(detail.deleted, [resolvedDelete]);
    assert.deepEqual(detail.gzipped, [resolvedGzip]);
    assert.ok(!JSON.stringify(detail).includes("link") && !JSON.stringify(detail).includes(".."));
  });
});

test("a future-dated directory is held out of the sweep instead of blocking it", async () => {
  await withTempDir(async (root) => {
    const fixtures = await writeTree(root);
    const future = join(root, "server", "2026-10-20", "events-09-100-001.jsonl");
    await put(future, "from the future\n".repeat(100));
    // So small a cap that `rotation.plan` reaches the future directory, which it counts as the
    // newest non-today directory — the case that used to make `validatePlan` refuse everything.
    const policy = { ...POLICY, maxTotalBytes: 1_000 };

    const { exit, emitted } = await runSweep(sweepOptions(root, { policy }));
    const report = succeeded(exit);

    assert.equal(await Deno.readTextFile(future), "from the future\n".repeat(100));
    assert.deepEqual(report.futureDated, ["server/2026-10-20"]);
    assert.ok(report.plan.keep.includes("server/2026-10-20/events-09-100-001.jsonl"));
    assert.ok(!report.deleted.includes("server/2026-10-20/events-09-100-001.jsonl"));
    // The rest of retention still ran: everything past keepDays is gone.
    for (const f of fixtures.filter((x) => x.age >= POLICY.keepDays)) {
      assert.equal(await exists(f.path), false, f.path);
    }
    const lifecycle = lifecycleEvents(emitted);
    assert.equal(lifecycle.length, 1);
    assert.equal(lifecycle[0].level, "warn");
    assert.deepEqual((lifecycle[0].detail as Record<string, unknown>).futureDated, ["server/2026-10-20"]);
    // The independent defence in depth is intact: a crafted plan that names it is still refused.
    const inv = await Effect.runPromise(inventory(root));
    const refused = await Effect.runPromiseExit(
      validatePlan(root, { gzip: [], delete: [future] }, new Set(inv.entries.map((e) => e.path)), TODAY),
    );
    const error = failureOf(refused);
    assert.ok(error instanceof RetentionPathError);
    assert.equal(error.reason, "today-or-future");
  });
});

test("an unsafe LOG_DIR is refused before anything is read", async () => {
  for (const logDir of ["/", ""]) {
    const { exit, emitted } = await runSweep(sweepOptions(logDir));
    const error = failureOf(exit);
    assert.ok(error instanceof RetentionPathError);
    assert.equal(error.reason, "root-unsafe");
    const errors = lifecycleEvents(emitted).filter((e) => e.level === "error");
    assert.equal(errors.length, 1);
    const detail = errors[0].detail as Record<string, unknown>;
    assert.deepEqual([detail.gzippedCount, detail.deletedCount, detail.keptCount], [0, 0, 0]);
  }
});

test("a file that changed between inventory and apply is skipped, not touched", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const grew = eventPath(root, "server", 5, 9, 100, 1);
    const stable = eventPath(root, "server", 20, 9, 100, 1);
    const inv = await Effect.runPromise(inventory(root));
    const verified = await verifiedFor([grew, stable]);
    const original = await Deno.readTextFile(grew);
    await Deno.writeTextFile(grew, "one more line\n", { append: true });

    const { exit } = await runEffect(Effect.gen(function* () {
      yield* TestClock.setTime(NOW.getTime());
      const exit = yield* Effect.exit(applyPlan({
        root,
        inventory: inv,
        plan: { gzip: [grew], delete: [stable], keep: [] },
        verified,
        today: TODAY,
        role: "cli",
      }));
      return { exit };
    }));
    const report = succeeded(exit);

    assert.deepEqual(report.skipped, [
      { path: rel(root, grew), action: "gzip", reason: "changed since inventory" },
    ]);
    assert.equal(await Deno.readTextFile(grew), `${original}one more line\n`);
    assert.equal(await exists(`${grew}.gz`), false);
    assert.equal(await exists(stable), false);
  });
});

/** `megabytes` of incompressible bytes: slow enough to gzip that a test can act mid-gzip. */
async function putRandom(path: string, megabytes: number): Promise<void> {
  const bytes = new Uint8Array(megabytes * 1024 * 1024);
  for (let at = 0; at < bytes.length; at += 65_536) crypto.getRandomValues(bytes.subarray(at, at + 65_536));
  await put(path, bytes);
}

/** Polls the real filesystem until `ready()` holds; the sweep does real I/O while `TestClock` stands still. */
const until = (ready: () => Promise<boolean>) =>
  Effect.gen(function* () {
    for (let i = 0; i < 5_000; i++) {
      if (yield* Effect.promise(ready)) return;
      yield* realPause(2);
    }
    return yield* Effect.die("timed out waiting for the filesystem");
  });

test("gzip rechecks the parent directory just before removing the source", async () => {
  await withTempDir(async (root) => {
    const source = join(root, "server", "2026-09-01", "events-09-100-001.jsonl");
    await putRandom(source, 32);
    const inv = await Effect.runPromise(inventory(root));
    const verified = await verifiedFor([source]);
    const moved = join(root, "server", "2026-09-01.moved");

    const exit = await runEffect(Effect.gen(function* () {
      yield* TestClock.setTime(NOW.getTime());
      const fiber = yield* Effect.fork(Effect.exit(applyPlan({
        root,
        inventory: inv,
        plan: { gzip: [source], delete: [], keep: [] },
        verified,
        today: TODAY,
        role: "cli",
      })));
      // Mid-gzip: swap the dated directory for a symlink. The open descriptors and the rename
      // still work, so only the recheck can notice that the path no longer means the same place.
      yield* until(() => exists(`${source}.gz.${Deno.pid}.tmp`));
      yield* Effect.promise(async () => {
        await Deno.rename(dirname(source), moved);
        await Deno.symlink(moved, dirname(source));
      });
      return yield* Fiber.join(fiber);
    }));
    const report = succeeded(exit);

    assert.deepEqual(report.gzipped, []);
    assert.equal(report.skipped.length, 1);
    assert.equal(report.skipped[0].action, "gzip");
    assert.equal(report.skipped[0].reason, "parent directory is not where the inventory found it");
    assert.equal(await exists(join(moved, "events-09-100-001.jsonl")), true, "the source survives");
  });
});

test("an interrupted sweep emits an audit event with the state it reached", async () => {
  await withTempDir(async (root) => {
    const small = [1, 2, 3].map((n) => join(root, "server", "2026-08-01", `events-0${n}-100-001.jsonl`));
    for (const path of small) await put(path, "old\n");
    const big = join(root, "server", "2026-09-01", "events-09-100-001.jsonl");
    await putRandom(big, 32);
    const inv = await Effect.runPromise(inventory(root));
    const verified = await verifiedFor([big, ...small]);

    const { emitted } = await runEffect(Effect.gen(function* () {
      yield* TestClock.setTime(NOW.getTime());
      const fiber = yield* Effect.fork(applyPlan({
        root,
        inventory: inv,
        plan: { gzip: [big], delete: small, keep: [] },
        verified,
        today: TODAY,
        role: "server",
      }));
      // Deletes run first, so a gzip in progress means the three deletes are done.
      yield* until(() => exists(`${big}.gz.${Deno.pid}.tmp`));
      yield* Fiber.interrupt(fiber);
      // The in-flight gzip is crash-safe and not abortable; let it land before the temp dir goes.
      yield* until(async () => !(await exists(big)));
      return { emitted: yield* events };
    }));

    const lifecycle = lifecycleEvents(emitted);
    assert.equal(lifecycle.length, 1);
    assert.equal(lifecycle[0].phase, `${PHASE}-interrupted`);
    assert.equal(lifecycle[0].level, "warn");
    assert.equal(lifecycle[0].role, "server");
    const detail = lifecycle[0].detail as Record<string, unknown>;
    assert.equal(detail.interrupted, true);
    assert.deepEqual(detail.deleted, small.map((p) => rel(root, p)).sort());
    assert.deepEqual(detail.gzipped, []);
    assert.equal(detail.inFlight, rel(root, big));
    assert.deepEqual(
      [detail.gzippedCount, detail.deletedCount, detail.keptCount],
      [0, 3, 1],
    );
  });
});

test("syncDir fsyncs a directory and treats an unsyncable or missing one as fine", async () => {
  await withTempDir(async (dir) => {
    assert.equal(await syncDir(dir), undefined);
    assert.equal(await syncDir(join(dir, "gone")), undefined);
    await put(join(dir, "f"), "x");
    assert.equal(await syncDir(join(dir, "f")), undefined);
    if (Deno.build.os === "linux") {
      // procfs refuses fsync on its directories with EINVAL: "not supported here", not a failure.
      assert.equal(await syncDir("/proc/self"), undefined);
    }
  });
});

test("applySafetyRails keeps open and quarantine paths, resolving them first", () => {
  const plan = {
    gzip: ["/l/server/d/a.jsonl", "/l/server/d/rejected.jsonl", "/l/server/d/open.jsonl"],
    delete: ["/l/server/e/open2.jsonl", "/l/server/e/b.jsonl"],
    keep: ["/l/server/f/c.jsonl"],
  };
  const railed = applySafetyRails(plan, ["/l/server/d/../d/open.jsonl", "/l/server/e/open2.jsonl"]);
  assert.deepEqual(railed.gzip, ["/l/server/d/a.jsonl"]);
  assert.deepEqual(railed.delete, ["/l/server/e/b.jsonl"]);
  assert.deepEqual(railed.keep, [
    "/l/server/d/open.jsonl",
    "/l/server/d/rejected.jsonl",
    "/l/server/e/open2.jsonl",
    "/l/server/f/c.jsonl",
  ]);
});

// ---------------------------------------------------------------------------
// The ingest gate (item 36), against hand-built ledgers
// ---------------------------------------------------------------------------

/** `diskLedger`, then `edit` applied to its watermarks. */
function editedLedger(
  root: string,
  edit: (watermarks: Map<string, CommittedWatermark>) => void,
): Effect.Effect<IngestLedger> {
  return Effect.map(diskLedger(root), (ledger) => {
    const watermarks = new Map(ledger.watermarks);
    edit(watermarks);
    return { ...ledger, watermarks };
  });
}

test("without committed watermarks nothing is gzipped or deleted, and all of it is reported", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const before = await snapshot(root);
    const empty: IngestLedger = { watermarks: new Map(), acceptedOffsets: () => Effect.succeed(new Map()) };

    const { exit, emitted } = await runSweep(sweepOptions(root, { ledger: Effect.succeed(empty) }));
    const report = succeeded(exit);

    assert.deepEqual(await snapshot(root), before);
    assert.deepEqual([report.gzipped, report.deleted, report.removedDirs], [[], [], []]);
    // Every event file the policy wanted acted on is backlog; quarantine-free days hold only events.
    assert.equal(report.blocked.length, 12 * 3 + 16 * 3 + 1);
    assert.ok(report.blocked.every((b) => b.reason === "no-watermark"));
    assert.equal(report.blockedBytes, report.blocked.reduce((sum, b) => sum + b.bytes, 0));
    const [event] = lifecycleEvents(emitted);
    assert.equal(event.level, "warn");
    assert.equal((event.detail as Record<string, unknown>).blockedCount, report.blocked.length);
  });
});

test("a short watermark pins its file; the cap then prunes the next directory instead", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const policy = { ...POLICY, keepDays: 100, gzipAfterDays: 100, maxTotalBytes: 30_000 };
    const oldest = eventPath(root, "server", 29, 9, 100, 1);
    const unpinned = rotationPlan((await Effect.runPromise(inventory(root))).entries, NOW, policy);
    const ledger = editedLedger(root, (w) => {
      const key = rel(root, oldest);
      const row = w.get(key)!;
      // Ingest stopped one line short: the last line is unread.
      w.set(key, { ...row, bytesIngested: row.bytesIngested - 10 });
    });

    const report = succeeded((await runSweep(sweepOptions(root, { policy, ledger }))).exit);

    assert.equal(await exists(oldest), true, "unread bytes are never deleted");
    assert.deepEqual(report.blocked, [
      {
        path: rel(root, oldest),
        action: "delete",
        reason: "unread-tail",
        bytes: (await Deno.stat(oldest)).size,
      },
    ]);
    // Its bytes still count, so the cap had to reach a date bucket the unpinned plan left alone.
    assert.ok(report.deleted.every((p) => p !== rel(root, oldest)));
    const extra = report.deleted.filter((p) => !unpinned.delete.includes(join(root, p)));
    assert.ok(extra.length > 0, "the cap moved on to a newer directory");
  });
});

test("a rejected.jsonl stays as long as a source beside it does", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const blockedSource = eventPath(root, "server", 20, 9, 100, 1);
    const quarantine = join(dirname(blockedSource), "rejected.jsonl");
    await putAged(quarantine, "{}\n", 20);
    const ledger = editedLedger(root, (w) => w.delete(rel(root, blockedSource)));

    const report = succeeded((await runSweep(sweepOptions(root, { ledger }))).exit);

    assert.equal(await exists(blockedSource), true);
    assert.equal(await exists(quarantine), true);
    const reasons = Object.fromEntries(report.blocked.map((b) => [b.path, b.reason]));
    assert.equal(reasons[rel(root, blockedSource)], "no-watermark");
    assert.equal(reasons[rel(root, quarantine)], "quarantine-evidence");
    // The other files of that day went; the directory stays for the two that could not.
    assert.equal(await exists(eventPath(root, "server", 20, 15, 100, 1)), false);
  });
});

test("a file modified within settleMs is not yet closed, whatever ingest says", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const recent = eventPath(root, "server", 20, 9, 100, 1);
    const touched = new Date(NOW.getTime() - 60_000);
    await Deno.utime(recent, touched, touched);

    const report = succeeded((await runSweep(sweepOptions(root, { settleMs: 5 * 60_000 }))).exit);

    assert.equal(await exists(recent), true);
    assert.deepEqual(report.blocked.map((b) => [b.path, b.reason]), [[rel(root, recent), "unsettled"]]);
    const settled = succeeded((await runSweep(sweepOptions(root, { settleMs: 30_000 }))).exit);
    assert.deepEqual(settled.deleted, [rel(root, recent)]);
  });
});

// ---------------------------------------------------------------------------
// Crash recovery
// ---------------------------------------------------------------------------

test("a complete .gz left by an interrupted sweep is verified, then the source is removed", async () => {
  await withTempDir(async (root) => {
    const fixtures = await writeTree(root);
    const source = eventPath(root, "server", 5, 9, 100, 1);
    const content = fixtures.find((f) => f.path === source)!.content;
    await put(`${source}.gz`, gzipSync(content));

    const report = succeeded((await runSweep(sweepOptions(root))).exit);

    assert.equal(await exists(source), false);
    assert.equal(gunzipSync(await Deno.readFile(`${source}.gz`)).toString("utf8"), content);
    assert.deepEqual(report.skipped, []);
  });
});

test("a .gz that does not match the source is left in place with the source, and reported", async () => {
  await withTempDir(async (root) => {
    const fixtures = await writeTree(root);
    const source = eventPath(root, "server", 5, 9, 100, 1);
    const content = fixtures.find((f) => f.path === source)!.content;
    const stranger = gzipSync("something else entirely\n");
    await put(`${source}.gz`, stranger);

    const { exit, emitted } = await runSweep(sweepOptions(root));
    const report = succeeded(exit);

    assert.equal(await Deno.readTextFile(source), content);
    assert.deepEqual(await Deno.readFile(`${source}.gz`), new Uint8Array(stranger));
    assert.deepEqual(report.skipped, [
      { path: rel(root, source), action: "gzip", reason: "a different .gz already exists" },
    ]);
    assert.equal(lifecycleEvents(emitted)[0].level, "warn");
  });
});

test("stale .tmp leftovers are swept; fresh ones and today's are not", async () => {
  await withTempDir(async (root) => {
    await writeTree(root);
    const age = (a: number, seq: number, mtimeOffsetMs: number) => ({
      path: `${eventPath(root, "server", a, 9, 100, seq)}.gz.999.tmp`,
      mtime: new Date(NOW.getTime() - mtimeOffsetMs),
    });
    const stale = age(5, 7, 2 * 60 * 60 * 1000);
    const fresh = age(5, 8, 60 * 1000);
    const today = age(0, 9, 2 * 60 * 60 * 1000);
    for (const f of [stale, fresh, today]) {
      await put(f.path, "partial");
      await Deno.utime(f.path, f.mtime, f.mtime);
    }

    succeeded((await runSweep(sweepOptions(root))).exit);

    assert.equal(await exists(stale.path), false);
    assert.equal(await exists(fresh.path), true);
    assert.equal(await exists(today.path), true);
  });
});

// ---------------------------------------------------------------------------
// The scheduled fiber
// ---------------------------------------------------------------------------

const waitForEvents = (count: number) =>
  Effect.gen(function* () {
    for (let i = 0; i < 500; i++) {
      const all = yield* events;
      if (all.length >= count) return all;
      // Real time on purpose: the sweep does real file I/O while `TestClock` stands still.
      yield* Effect.promise(() => new Promise((resolve) => setTimeout(resolve, 10)));
    }
    return yield* Effect.die(`timed out waiting for ${count} events`);
  });

const realPause = (ms: number) => Effect.promise(() => new Promise((resolve) => setTimeout(resolve, ms)));

test("the sweeper fiber sweeps at startup, then once a day, from Config", async () => {
  await withTempDir(async (root) => {
    const fixtures = await writeTree(root);
    const config = ConfigTest({
      logging: { dir: root, retentionDays: 14, gzipAfterDays: 2, maxTotalBytes: Number.MAX_SAFE_INTEGER },
    });
    const late = eventPath(root, "server", 20, 4, 55, 1);

    await runEffect(
      Effect.gen(function* () {
        yield* TestClock.setTime(NOW.getTime());
        yield* startRetentionSweeper({ role: "server", ledger: diskLedger(root) });

        const first = yield* waitForEvents(1);
        assert.equal(first.length, 1);
        assert.deepEqual(yield* Effect.promise(() => listFiles(root)), expectedAfterSweep(root, fixtures));

        // New garbage appears; the next sweep is a day away, so it survives a 23-hour wait.
        yield* Effect.promise(() => putAged(late, "late\n", 20, 4));
        yield* realPause(50);
        yield* TestClock.adjust(Duration.hours(23));
        yield* realPause(100);
        assert.equal((yield* events).length, 1);
        assert.equal(yield* Effect.promise(() => exists(late)), true);

        yield* TestClock.adjust(Duration.hours(1));
        const second = yield* waitForEvents(2);
        assert.equal(second.length, 2);
        assert.equal(yield* Effect.promise(() => exists(late)), false);
        assert.equal(second[1].role, "server");
      }),
      config,
    );
  });
});

test("a failing sweep is an error event and does not stop tomorrow's sweep", async () => {
  const config = ConfigTest({ logging: { dir: "/" } });

  await runEffect(
    Effect.gen(function* () {
      yield* TestClock.setTime(NOW.getTime());
      yield* startRetentionSweeper({ role: "server", ledger: diskLedger("/") });

      yield* waitForEvents(1);
      yield* realPause(50);
      yield* TestClock.adjust(Duration.days(1));
      const all = yield* waitForEvents(2);

      const errors = lifecycleEvents(all);
      assert.equal(errors.length, 2);
      for (const e of errors) {
        assert.equal(e.level, "error");
        assert.equal((e.detail as Record<string, unknown>).errorTag, "RetentionPathError");
      }
    }),
    config,
  );
});
