/**
 * FFI-07, work 1: value round trips through the FFI binding (`platform/sqlite_ffi/`) agree with
 * `node:sqlite`, the independent oracle, apart from differences this file pins by name.
 *
 * For a generated value of every column kind, `bind -> step -> column` runs three ways on each side:
 * `SELECT ?1, typeof(?1)` (a parameter straight back out), an INSERT into a STRICT `ANY` column
 * followed by a SELECT of it, and, on the binding only, the same SELECT through `getAsync` (a pool
 * thread). The binding's answers must match the oracle's, with these exceptions, each asserted as
 * a difference rather than skipped, so a change in either direction fails a test:
 *
 * 1. **U+0000 text (ADR 0005).** The binding reads TEXT by its byte length and returns the whole
 *    string; `node:sqlite` returns it truncated at the first NUL. Both bind it intact.
 * 2. **Integral doubles bind as INTEGER.** `node:sqlite` binds every JS `number` as REAL; the binding
 *    binds an integral safe number as INTEGER (`docs/knowledge/gotchas.md`, FFI-04). The two agree
 *    on the number, not on `typeof()`. Compared by value.
 * 3. **`-0` loses its sign.** The binding's int32 fast path excludes `-0`, but the next branch,
 *    `Number.isSafeInteger(-0)`, then binds it as the INTEGER 0, so `SELECT ?` returns integer `0`
 *    where `node:sqlite` returns real `-0`. Found by this property. Not fixed here (`platform/` is
 *    outside FFI-07); the test pins today's behaviour, and a fix that binds `-0` as a double makes
 *    `pinned: -0 ...` fail, which is the signal to move `-0` into the agreeing group. In a `REAL`
 *    column both sides store the same value, so only an untyped (`ANY`/expression) column shows it.
 *
 * Where the two agree on something the plan did not expect: `NaN` is **not** an error in either
 * driver. SQLite's `sqlite3_bind_double` turns NaN into NULL, so both return `null`. Pinned.
 * Lone surrogates are not rejected either: both drivers encode them as U+FFFD, so the round trip
 * equals `toWellFormed()` of the input (the generators produce them on purpose).
 *
 * Integers: `readBigInts` on both sides, so every int64 is exact, plus a second property without it:
 * safe integers read as `number`, the rest fail `SqliteValueError("unsafe-integer")` on the binding
 * and throw on the oracle. Bigints outside int64 are rejected by both, typed on the binding.
 *
 * The one rule (FFI plan §1.2, `docs/knowledge/gotchas.md`): the oracle runs on its own `:memory:`
 * database and the binding on its own temp file. They never touch the same file, so this file may
 * import the second SQLite copy (`tests/unit/boundaries_test.ts` allow-list).
 *
 * `numRuns: 200`, seed from `PROP_SEED` (default 20261010). Sweep:
 * `bash work-scripts/bash/property-seed-sweep.sh tests/property/ffi_roundtrip_test.ts - 1 200`.
 */
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import fc from "fast-check";
import { Database, SqliteValueError } from "../../platform/sqlite_ffi/mod.ts";
import { SEED, titled } from "./arbitraries.ts";
import { runs } from "./compiler_arbitraries.ts";

const RUNS = 200;
const INT64_MIN = -(2n ** 63n);
const INT64_MAX = 2n ** 63n - 1n;

// ---------------------------------------------------------------------------
// Generators
// ---------------------------------------------------------------------------

const surrogate = fc.integer({ min: 0xd800, max: 0xdfff }).map((c) => String.fromCharCode(c));
const NUL = "\u0000";

/** Text: full Unicode, NULs at any position, lone surrogates, and the fixed edge cases. */
const textArb: fc.Arbitrary<string> = fc.oneof(
  {
    weight: 2,
    arbitrary: fc.constantFrom("", NUL, "a\0b", "\0\0", "\ud800", "x\udfffy", "\u{1F600}\0\u{1F600}"),
  },
  { weight: 3, arbitrary: fc.string({ unit: "binary", maxLength: 24 }) },
  { weight: 2, arbitrary: fc.string({ maxLength: 24 }) },
  // NUL spliced between two arbitrary halves: always U+0000 text (the pinned difference).
  {
    weight: 3,
    arbitrary: fc.tuple(fc.string({ maxLength: 8 }), fc.string({ maxLength: 8 })).map(([a, b]) =>
      a + NUL + b
    ),
  },
  // Lone surrogates mixed with ordinary text.
  {
    weight: 3,
    arbitrary: fc.array(fc.oneof(surrogate, fc.string({ unit: "binary", maxLength: 3 })), { maxLength: 6 })
      .map((parts) => parts.join("")),
  },
);

/** int64, with the boundaries over-represented, as `bigint`. */
const intArb: fc.Arbitrary<bigint> = fc.oneof(
  { weight: 3, arbitrary: fc.bigInt({ min: INT64_MIN, max: INT64_MAX }) },
  {
    weight: 2,
    arbitrary: fc.constantFrom(
      INT64_MIN,
      INT64_MAX,
      0n,
      1n,
      -1n,
      2n ** 53n - 1n,
      -(2n ** 53n - 1n),
      2n ** 53n,
      -(2n ** 53n),
      2n ** 53n + 1n,
      2n ** 31n,
      -(2n ** 31n) - 1n,
    ),
  },
  { weight: 2, arbitrary: fc.integer().map(BigInt) },
);

/** Doubles: the default `fc.double` yields NaN, both infinities, `-0` and subnormals; plus safe integers. */
const doubleArb: fc.Arbitrary<number> = fc.oneof(
  { weight: 4, arbitrary: fc.double() },
  { weight: 3, arbitrary: fc.constantFrom(-0, 0, NaN, Infinity, -Infinity, 0.5, 1e21, 2 ** 53, -(2 ** 53)) },
  { weight: 2, arbitrary: fc.maxSafeInteger() },
  { weight: 1, arbitrary: fc.integer() },
);

const blobArb: fc.Arbitrary<Uint8Array> = fc.oneof(
  { weight: 1, arbitrary: fc.constant(new Uint8Array(0)) },
  { weight: 4, arbitrary: fc.uint8Array({ maxLength: 48 }) },
);

type Case =
  | { kind: "text"; value: string }
  | { kind: "int"; value: bigint }
  | { kind: "double"; value: number }
  | { kind: "blob"; value: Uint8Array }
  | { kind: "null"; value: null };

const caseArb: fc.Arbitrary<Case> = fc.oneof(
  textArb.map((value): Case => ({ kind: "text", value })),
  intArb.map((value): Case => ({ kind: "int", value })),
  doubleArb.map((value): Case => ({ kind: "double", value })),
  blobArb.map((value): Case => ({ kind: "blob", value })),
  fc.constant<Case>({ kind: "null", value: null }),
);

// ---------------------------------------------------------------------------
// The two drivers, each on a database of its own
// ---------------------------------------------------------------------------

type Read = { v: unknown; t: string };

const DDL = "CREATE TABLE t(id INTEGER PRIMARY KEY, v ANY) STRICT";
const PARAM = "SELECT ?1 AS v, typeof(?1) AS t";
/** Two parameters: `node:sqlite` reads a lone object argument as named parameters, not as a value. */
const PAIR = "SELECT ?1 AS v, typeof(?2) AS t";
const STORED = "SELECT v, typeof(v) AS t FROM t WHERE id = ?";

type Rig = {
  oracle: DatabaseSync;
  binding: Database;
  /** The same value through every path of one driver. */
  oracleReads(value: unknown): Read[];
  bindingReads(value: unknown): Read[];
  bindingAsyncRead(value: unknown): Promise<Read>;
  nextId(): number;
};

function plainRead(row: unknown): Read {
  const r = row as { v: unknown; t: string };
  return { v: r.v, t: r.t };
}

async function withRig<T>(
  options: { readBigInts: boolean },
  fn: (rig: Rig) => Promise<T>,
): Promise<T> {
  const dir = mkdtempSync(join(tmpdir(), "mansio-ffi-roundtrip-"));
  // The oracle is `:memory:`; the binding has the temp file. No file is shared (FFI plan §1.2).
  const oracle = new DatabaseSync(":memory:");
  const binding = Database.open(join(dir, "binding.db"), options);
  let id = 0;
  try {
    oracle.exec(DDL);
    binding.exec(DDL);
    const oracleParam = oracle.prepare(PARAM);
    const oracleInsert = oracle.prepare("INSERT INTO t(id, v) VALUES (?, ?)");
    const oracleStored = oracle.prepare(STORED);
    // node:sqlite sets this per statement, not per connection.
    for (const statement of [oracleParam, oracleStored]) statement.setReadBigInts(options.readBigInts);
    const bindingParam = binding.prepare(PARAM);
    const bindingInsert = binding.prepare("INSERT INTO t(id, v) VALUES (?, ?)");
    const bindingStored = binding.prepare(STORED);
    return await fn({
      oracle,
      binding,
      oracleReads(value) {
        const n = ++id;
        oracleInsert.run(n, value as never);
        return [plainRead(oracleParam.get(value as never)), plainRead(oracleStored.get(n))];
      },
      bindingReads(value) {
        const n = ++id;
        bindingInsert.run([n, value as never]);
        return [plainRead(bindingParam.get([value as never])), plainRead(bindingStored.get([n]))];
      },
      async bindingAsyncRead(value) {
        return plainRead(await bindingParam.getAsync([value as never]));
      },
      nextId: () => id,
    });
  } finally {
    await binding.closeAsync();
    oracle.close();
    rmSync(dir, { recursive: true, force: true });
  }
}

// ---------------------------------------------------------------------------
// Expectations
// ---------------------------------------------------------------------------

const wellFormed = (s: string): string => s.toWellFormed();

/** What the bound `number` must read back as, per the three groups in the module comment. */
function expectNumber(
  x: number,
  oracle: Read,
  binding: Read,
  seen: Record<string, number>,
): void {
  const label = Object.is(x, -0) ? "-0" : String(x);
  if (Number.isNaN(x)) {
    // SQLite turns a NaN bind into NULL, in both drivers: not a typed error.
    seen.nan++;
    assert.deepStrictEqual([oracle, binding], [{ v: null, t: "null" }, { v: null, t: "null" }], label);
  } else if (Object.is(x, -0)) {
    // pinned: -0 -- the binding binds INTEGER 0, node:sqlite binds REAL -0.
    seen.negativeZero++;
    assert.deepStrictEqual(oracle, { v: -0, t: "real" }, "pinned: -0 oracle");
    assert.deepStrictEqual(binding, { v: 0n, t: "integer" }, "pinned: -0 binding (loses its sign)");
  } else if (Number.isSafeInteger(x)) {
    // Integral safe doubles: same number, different storage class (see module comment, 2).
    seen.integral++;
    assert.deepStrictEqual(oracle, { v: x, t: "real" }, `oracle ${label}`);
    assert.deepStrictEqual(binding, { v: BigInt(x), t: "integer" }, `binding ${label}`);
  } else {
    // Non-integral, unsafe, infinite: REAL on both sides, bit for bit.
    seen.real++;
    assert.ok(
      typeof oracle.v === "number" && Object.is(oracle.v, x) && oracle.t === "real",
      `oracle ${label}`,
    );
    assert.ok(
      typeof binding.v === "number" && Object.is(binding.v, x) && binding.t === "real",
      `binding ${label}`,
    );
  }
}

function expectCase(
  c: Case,
  oracle: Read,
  binding: Read,
  seen: Record<string, number>,
): void {
  switch (c.kind) {
    case "text": {
      const whole = wellFormed(c.value);
      assert.equal(binding.t, "text");
      assert.equal(oracle.t, "text");
      if (whole.includes(NUL)) {
        // ADR 0005: node:sqlite truncates at the first NUL, the binding returns the whole string.
        seen.nul++;
        assert.equal(oracle.v, whole.slice(0, whole.indexOf(NUL)), "pinned: node:sqlite truncates at U+0000");
        assert.equal(binding.v, whole, "pinned: the binding returns U+0000 text whole");
      } else {
        if (c.value !== whole) seen.surrogate++;
        assert.equal(binding.v, whole, "binding text");
        assert.equal(oracle.v, whole, "oracle text");
      }
      return;
    }
    case "int":
      seen.int++;
      assert.deepStrictEqual(oracle, { v: c.value, t: "integer" });
      assert.deepStrictEqual(binding, { v: c.value, t: "integer" });
      return;
    case "double":
      return expectNumber(c.value, oracle, binding, seen);
    case "blob":
      seen.blob++;
      assert.equal(oracle.t, "blob");
      assert.equal(binding.t, "blob");
      assert.deepStrictEqual(oracle.v, c.value);
      assert.deepStrictEqual(binding.v, c.value);
      return;
    case "null":
      assert.deepStrictEqual([oracle, binding], [{ v: null, t: "null" }, { v: null, t: "null" }]);
  }
}

/** One value through every path of both drivers, checked against the expectations above. */
async function roundTrip(rig: Rig, c: Case, seen: Record<string, number>): Promise<void> {
  const oracleReads = rig.oracleReads(c.value);
  const bindingReads = rig.bindingReads(c.value);
  // Direct parameter read and stored-column read must agree with each other on each side.
  assert.deepStrictEqual(oracleReads[0], oracleReads[1], "oracle: parameter vs stored column");
  assert.deepStrictEqual(bindingReads[0], bindingReads[1], "binding: parameter vs stored column");
  expectCase(c, oracleReads[0], bindingReads[0], seen);
  // The pool-thread step reads the same value as the main-thread step.
  assert.deepStrictEqual(await rig.bindingAsyncRead(c.value), bindingReads[0], "getAsync vs get");
}

const zero = () => ({ nan: 0, negativeZero: 0, integral: 0, real: 0, nul: 0, surrogate: 0, int: 0, blob: 0 });

// ---------------------------------------------------------------------------
// Properties
// ---------------------------------------------------------------------------

Deno.test(
  titled(
    `ffi round trip: every column kind agrees with node:sqlite, pinned differences named (${RUNS} runs)`,
  ),
  async () => {
    const seen = zero();
    await withRig({ readBigInts: true }, async (rig) => {
      await fc.assert(
        fc.asyncProperty(caseArb, (c) => roundTrip(rig, c, seen)),
        runs(RUNS),
      );
    });
    // The generators reached every group (seed-independent lower bounds over 200 runs).
    console.log(`round trip coverage (seed ${SEED}): ${JSON.stringify(seen)}`);
    assert.ok(
      seen.nul >= 5 && seen.int >= 5 && seen.blob >= 5,
      `generators too narrow: ${JSON.stringify(seen)}`,
    );
    assert.ok(seen.integral + seen.real >= 5, `no doubles generated: ${JSON.stringify(seen)}`);
  },
);

Deno.test("ffi round trip: the edge values, one by one (NaN is NULL, -0 loses its sign, NUL text is whole)", async () => {
  const seen = zero();
  const edges: Case[] = [
    ...[
      NaN,
      -0,
      0,
      Infinity,
      -Infinity,
      Number.MIN_VALUE,
      -Number.MIN_VALUE,
      Number.MAX_VALUE,
      2 ** 53,
      -(2 ** 53),
      2 ** 63,
      0.1,
    ]
      .map((value): Case => ({ kind: "double", value })),
    ...[Number.MAX_SAFE_INTEGER, Number.MIN_SAFE_INTEGER, 1, -1, 2 ** 31, -(2 ** 31) - 1]
      .map((value): Case => ({ kind: "double", value })),
    ...[INT64_MIN, INT64_MAX, 0n, 2n ** 53n].map((value): Case => ({ kind: "int", value })),
    ...["", NUL, "a\0b", "\ud800", "\udc00\ud800", "\u{1F600}"].map((value): Case => ({
      kind: "text",
      value,
    })),
    { kind: "blob", value: new Uint8Array(0) },
    { kind: "blob", value: new Uint8Array([0, 1, 255]) },
    { kind: "null", value: null },
  ];
  await withRig({ readBigInts: true }, async (rig) => {
    for (const c of edges) await roundTrip(rig, c, seen);
  });
  assert.equal(seen.nan, 1);
  assert.equal(seen.negativeZero, 1);
  assert.equal(seen.nul, 2, "NUL and a\\0b are the two U+0000 strings");
  assert.equal(seen.surrogate, 2);
});

Deno.test(
  titled(`ffi round trip: without readBigInts, safe integers are numbers and the rest fail (${RUNS} runs)`),
  async () => {
    let unsafe = 0;
    await withRig({ readBigInts: false }, async (rig) => {
      await fc.assert(
        fc.asyncProperty(intArb, async (x) => {
          const safe = x >= -(2n ** 53n - 1n) && x <= 2n ** 53n - 1n;
          if (safe) {
            // Both drivers hand back the exact number; `-0` cannot arise from a bigint.
            const expected = { v: Number(x), t: "integer" };
            for (const read of [...rig.oracleReads(x), ...rig.bindingReads(x)]) {
              assert.deepStrictEqual(read, expected);
            }
            return;
          }
          unsafe++;
          // Oracle: refuses the read, except INT64_MIN, which its overflow check lets through as the
          // lossy double -2^63 (an oracle defect, pinned so the exception stays visible).
          if (x === INT64_MIN) {
            assert.equal(rig.oracleReads(x)[0].v, -(2 ** 63), "pinned: node:sqlite reads INT64_MIN lossily");
          } else {
            assert.throws(() => rig.oracleReads(x), Error, `oracle accepted ${x}`);
          }
          // Binding: the typed `unsafe-integer` error, with no exceptions.
          const failure = await rig.bindingAsyncRead(x).then(() => undefined, (e: unknown) => e);
          assert.ok(failure instanceof SqliteValueError, `binding did not fail typed for ${x}: ${failure}`);
          assert.equal(failure.reason, "unsafe-integer");
          // The connection answers the next statement.
          assert.equal(rig.binding.prepare("SELECT 41 + 1 AS x").get()?.x, 42);
        }),
        runs(RUNS),
      );
    });
    assert.ok(unsafe >= 5, `generated only ${unsafe} unsafe integers`);
  },
);

Deno.test(
  titled(`ffi round trip: values that cannot cross the boundary fail on both sides (${RUNS} runs)`),
  async () => {
    // Outside int64, and values no SQLite storage class can hold. NaN is deliberately absent: it is
    // NULL, not an error (pinned in the first property).
    const outOfRange = fc.oneof(
      fc.bigInt({ min: INT64_MAX + 1n, max: 2n ** 70n }),
      fc.bigInt({ min: -(2n ** 70n), max: INT64_MIN - 1n }),
      fc.constantFrom(INT64_MAX + 1n, INT64_MIN - 1n),
    );
    const unsupported = fc.constantFrom<unknown>(undefined, {}, [], [1], () => 1, Symbol("s"));
    await withRig({ readBigInts: true }, async (rig) => {
      await fc.assert(
        fc.asyncProperty(fc.oneof(outOfRange, unsupported), async (value) => {
          assert.throws(() => rig.oracle.prepare(PAIR).get(1, value as never), Error, "oracle accepted it");
          let failure: unknown;
          try {
            rig.binding.prepare(PAIR).get([1, value as never]);
          } catch (e) {
            failure = e;
          }
          assert.ok(failure instanceof SqliteValueError, `binding failure was ${String(failure)}`);
          assert.equal(
            failure.reason,
            typeof value === "bigint" ? "integer-out-of-range" : "unsupported-bind-type",
          );
          // The failed bind left no half-bound statement behind: the connection is reusable.
          assert.deepStrictEqual(plainRead(rig.binding.prepare(PARAM).get([7n])), { v: 7n, t: "integer" });
          assert.deepStrictEqual(plainRead(await rig.binding.prepare(PARAM).getAsync(["ok"])), {
            v: "ok",
            t: "text",
          });
        }),
        runs(RUNS),
      );
    });
  },
);
