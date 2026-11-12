/**
 * fast-check generators for the schema layer, plus **the** seed harness for every
 * property suite under `tests/property/`.
 *
 * ## Seed harness
 *
 * Every property suite imports this module (directly, or through `compiler_arbitraries.ts`), and this
 * is the only module that calls `fc.configureGlobal`. Importing it sets, for every `fc.assert` in the
 * process:
 *
 * - a fixed seed, `DEFAULT_SEED`, so a failure reproduces bit-for-bit;
 * - `numRuns: NUM_RUNS` (200), which a suite may lower per assert for expensive properties;
 * - a reporter that, on failure, appends a `PROP_SEED=<seed> deno task test:prop` replay line to
 *   fast-check's counterexample report (which already carries the seed, the shrink path and the
 *   counterexample).
 *
 * `PROP_SEED=<int>` is the single override: set it to explore new cases, or to replay a red run. Use
 * `titled()` for test names so the seed in use is printed next to every test, pass or fail.
 *
 * Recursive generators take depth as an explicit parameter
 * (`nodeArb(maxDepth)`), not `fc.letrec`, so nesting is bounded by construction and shrinks cleanly.
 *
 * Nothing here restates the validator. "Matching" values are built structurally from the node, and
 * "mismatching" values are built from a known-wrong shape *together with* the path `validate` is expected
 * to report, so a property over them is falsifiable in both directions.
 */
import fc from "fast-check";
import {
  defineSchema,
  defineTable,
  type Field,
  type Fields,
  type Node,
  type Schema,
  type Table,
} from "../../core/schema.ts";
import { Either } from "effect";

// ---------------------------------------------------------------------------
// Seed and run count
// ---------------------------------------------------------------------------

/** Default seed. Recorded in every test name so a CI failure can be replayed locally. */
export const DEFAULT_SEED = 20261010;

function readSeed(): number {
  try {
    const raw = Deno.env.get("PROP_SEED");
    if (raw !== undefined && Number.isInteger(Number(raw))) return Number(raw);
  } catch {
    // No --allow-env: fall back to the recorded default.
  }
  return DEFAULT_SEED;
}

export const SEED: number = readSeed();
/** Runs per property. The plan's acceptance bar is >= 200. */
export const NUM_RUNS = 200;

/** The command that replays a run with `seed`. Printed under every failing property's report. */
export function replayCommand(seed: number): string {
  return `PROP_SEED=${seed} deno task test:prop`;
}

/**
 * fast-check's default failure, plus the replay command. Installed globally, so it covers `fc.assert`
 * calls that pass their own parameters too; fast-check routes async properties here as well when no
 * `asyncReporter` is set.
 */
function reporter(details: fc.RunDetails<unknown>): void {
  if (!details.failed) return;
  const message = `${fc.defaultReportMessage(details)}\n\nReplay: ${replayCommand(details.seed)}`;
  throw new Error(message, { cause: details.errorInstance });
}

fc.configureGlobal({ seed: SEED, numRuns: NUM_RUNS, reporter });

/** Suffixes a test title with the configured seed, so the seed shows in `deno test` output. */
export function titled(title: string): string {
  return `${title} [seed=${SEED}]`;
}

/** Runs one property with the suite's run count. A failure message carries the shrunk counterexample. */
export function check<T>(property: fc.IPropertyWithHooks<T>): void {
  fc.assert(property, { numRuns: NUM_RUNS });
}

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

const IDENT_START = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_".split("");
const IDENT_REST = [...IDENT_START, ..."0123456789".split("")];
/** Prototype-chain names are valid identifiers; they must never be mistaken for own fields. */
const PROTOTYPE_NAMES = ["constructor", "toString", "hasOwnProperty", "valueOf", "toJSON"];

/** Valid SQL identifiers: a letter or `_`, then letters, digits and `_`. */
export const identifierArb: fc.Arbitrary<string> = fc.oneof(
  {
    weight: 8,
    arbitrary: fc.tuple(
      fc.constantFrom(...IDENT_START),
      fc.string({ unit: fc.constantFrom(...IDENT_REST), maxLength: 12 }),
    ).map(([h, t]) => h + t),
  },
  { weight: 1, arbitrary: fc.constantFrom(...PROTOTYPE_NAMES) },
);

/** Valid identifiers that are also legal as a field or table name (no `_` prefix, no `sqlite_` prefix). */
export const publicNameArb: fc.Arbitrary<string> = identifierArb.filter((s) =>
  !s.startsWith("_") && !s.toLowerCase().startsWith("sqlite_")
);

/** Characters that must never survive into interpolated SQL. */
const BAD_CHARS = [
  " ",
  "-",
  ".",
  ";",
  "'",
  '"',
  "`",
  "(",
  ")",
  "*",
  "/",
  "\\",
  "\n",
  "\t",
  "\0",
  "é",
  "日",
  "😀",
  "$",
  "%",
  " ",
];

/** Invalid identifiers, built by construction: empty, digit-led, or a valid name with a bad character spliced in. */
export const invalidIdentifierArb: fc.Arbitrary<string> = fc.oneof(
  fc.constant(""),
  fc.tuple(
    fc.constantFrom(..."0123456789".split("")),
    fc.string({ unit: fc.constantFrom(...IDENT_REST), maxLength: 6 }),
  )
    .map(([d, t]) => d + t),
  fc.tuple(
    identifierArb,
    fc.constantFrom(...BAD_CHARS),
    fc.string({ unit: fc.constantFrom(...IDENT_REST), maxLength: 6 }),
  )
    .map(([head, bad, tail]) => head + bad + tail),
  // Injection-shaped: a valid-looking name followed by a statement.
  fc.tuple(identifierArb, fc.constantFrom("; DROP TABLE t", '" OR 1=1 --', "'); --", "\nSELECT 1"))
    .map(([head, tail]) => head + tail),
);

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

const SCALAR_KINDS = ["string", "number", "integer", "boolean"] as const;

const nullableArb: fc.Arbitrary<{ nullable?: true }> = fc.boolean().map((
  n,
) => (n ? { nullable: true as const } : {}));

/** A node with no array and no further nesting. */
export const leafNodeArb: fc.Arbitrary<Node> = fc.oneof(
  fc.tuple(fc.constantFrom(...SCALAR_KINDS), nullableArb).map(([kind, n]): Node => ({ kind, ...n })),
  fc.tuple(fc.uniqueArray(fc.string({ maxLength: 6 }), { minLength: 1, maxLength: 4 }), nullableArb)
    .map(([values, n]): Node => ({ kind: "enum", values, ...n })),
);

/**
 * Nodes of every kind × nullable × array nesting up to `maxDepth` (array-of-array-of-scalar at 2).
 * Leaves are weighted 3:1 over arrays at every level so schemas stay small.
 */
export function nodeArb(maxDepth = 2): fc.Arbitrary<Node> {
  if (maxDepth <= 0) return leafNodeArb;
  const arrayNode = fc.tuple(
    nodeArb(maxDepth - 1),
    fc.option(fc.integer({ min: 0, max: 4 }), { nil: undefined }),
    nullableArb,
  ).map(([item, length, n]): Node => ({
    kind: "array",
    item,
    ...(length === undefined ? {} : { length }),
    ...n,
  }));
  return fc.oneof({ weight: 3, arbitrary: leafNodeArb }, { weight: 1, arbitrary: arrayNode });
}

/** Number of array levels above the deepest leaf. Used by tests to assert the depth bound. */
export function arrayDepth(node: Node): number {
  return node.kind === "array" && node.item ? 1 + arrayDepth(node.item) : 0;
}

// ---------------------------------------------------------------------------
// Values matching / not matching a node
// ---------------------------------------------------------------------------

const safeInteger = fc.integer({ min: Number.MIN_SAFE_INTEGER, max: Number.MAX_SAFE_INTEGER });
const finiteDouble = fc.double({ noNaN: true, noDefaultInfinity: true });

function baseValueArb(node: Node): fc.Arbitrary<unknown> {
  switch (node.kind) {
    case "string":
      return fc.string({ maxLength: 12 });
    case "number":
      return finiteDouble;
    case "integer":
      return fc.oneof(fc.integer({ min: -1000, max: 1000 }), safeInteger);
    case "boolean":
      return fc.boolean();
    case "enum":
      return fc.constantFrom(...(node.values ?? []));
    case "array": {
      const item = matchingValueArb(node.item as Node);
      return node.length === undefined
        ? fc.array(item, { maxLength: 4 })
        : fc.array(item, { minLength: node.length, maxLength: node.length });
    }
  }
}

/** Values the node accepts. `null` is included (weighted) exactly when the node is nullable. */
export function matchingValueArb(node: Node): fc.Arbitrary<unknown> {
  const base = baseValueArb(node);
  return node.nullable
    ? fc.oneof({ weight: 1, arbitrary: fc.constant(null) }, { weight: 4, arbitrary: base })
    : base;
}

/** A value the node rejects, and the `path` `validate` must blame for it when called with `path`. */
export type Mismatch = { value: unknown; path: string };

/** Shapes no scalar node of any kind accepts, plus `null` when the node is not nullable. */
function foreignArb(node: Node): fc.Arbitrary<unknown> {
  const always: fc.Arbitrary<unknown>[] = [
    fc.constant(undefined),
    fc.array(fc.integer(), { maxLength: 2 }),
    fc.dictionary(fc.string({ maxLength: 4 }), fc.integer(), { maxKeys: 2 }),
    fc.bigInt(),
  ];
  if (!node.nullable) always.push(fc.constant(null));
  return fc.oneof(...always);
}

/**
 * Values `node` rejects, paired with the expected failure path. Array element failures report the
 * *first* failing index, so the generator emits matching elements before the bad one.
 */
export function mismatchArb(node: Node, path: string): fc.Arbitrary<Mismatch> {
  const at = (value: fc.Arbitrary<unknown>): fc.Arbitrary<Mismatch> => value.map((v) => ({ value: v, path }));
  const foreign = at(foreignArb(node));
  switch (node.kind) {
    case "string":
      return fc.oneof(foreign, at(fc.oneof(fc.double(), fc.boolean())));
    case "number":
      return fc.oneof(
        foreign,
        at(fc.oneof(fc.string({ maxLength: 6 }), fc.boolean(), fc.constantFrom(NaN, Infinity, -Infinity))),
      );
    case "integer":
      return fc.oneof(
        foreign,
        at(fc.oneof(
          fc.string({ maxLength: 6 }),
          fc.boolean(),
          fc.constantFrom(NaN, Infinity, -Infinity, 0.5, -1.5, 2 ** 53, -(2 ** 53), 1e300),
          fc.double().filter((n) => !Number.isSafeInteger(n)),
        )),
      );
    case "boolean":
      return fc.oneof(
        foreign,
        at(fc.oneof(fc.string({ maxLength: 6 }), fc.double(), fc.constantFrom("true", "false", 0, 1))),
      );
    case "enum": {
      const values = node.values ?? [];
      const outsider = fc.oneof(
        fc.string({ maxLength: 8 }).filter((s) => !values.includes(s)),
        // Near-misses: a declared value with a character appended. A finite value set is never closed
        // under appending, so this filter always has a way to succeed (unlike e.g. `toUpperCase`).
        fc.constantFrom(...values).map((s) => s + "\u0000").filter((s) => !values.includes(s)),
      );
      return fc.oneof(foreign, at(fc.oneof(outsider, fc.double(), fc.boolean())));
    }
    case "array": {
      const item = node.item as Node;
      const notAnArray = at(
        fc.oneof(
          fc.string({ maxLength: 6 }),
          fc.double(),
          fc.boolean(),
          fc.constant(undefined),
          fc.dictionary(fc.string({ maxLength: 4 }), fc.integer(), { maxKeys: 2 }),
          fc.bigInt(),
          ...(node.nullable ? [] : [fc.constant(null)]),
        ),
      );
      const elementCount = node.length === undefined
        ? fc.integer({ min: 1, max: 4 })
        : fc.constant(node.length);
      // A right-length array whose element `i` is the first bad one.
      const badElement = node.length === 0 ? [] : [
        elementCount.filter((n) => n > 0).chain((n) =>
          fc.integer({ min: 0, max: n - 1 }).chain((i) =>
            fc.tuple(
              fc.array(matchingValueArb(item), { minLength: i, maxLength: i }),
              mismatchArb(item, `${path}[${i}]`),
              fc.array(matchingValueArb(item), { minLength: n - 1 - i, maxLength: n - 1 - i }),
            ).map(([head, bad, tail]): Mismatch => ({ value: [...head, bad.value, ...tail], path: bad.path }))
          )
        ),
      ];
      // A matching-element array whose length is wrong (only meaningful when a length is declared).
      const wrongLength = node.length === undefined ? [] : [
        fc.integer({ min: 0, max: node.length + 3 }).filter((n) => n !== node.length).chain((n) =>
          at(fc.array(matchingValueArb(item), { minLength: n, maxLength: n }))
        ),
      ];
      return fc.oneof(
        { weight: 1, arbitrary: notAnArray },
        ...badElement.map((arbitrary) => ({ weight: 3, arbitrary })),
        ...wrongLength.map((arbitrary) => ({ weight: 2, arbitrary })),
      );
    }
  }
}

// ---------------------------------------------------------------------------
// Fields, Table, Schema
// ---------------------------------------------------------------------------

/** A field map with 0..`maxFields` distinct, valid, unreserved names. */
export function fieldsArb(nodeDepth = 2, maxFields = 5): fc.Arbitrary<Fields> {
  return fc.uniqueArray(fc.tuple(publicNameArb, nodeArb(nodeDepth)), {
    selector: ([name]) => name,
    minLength: 0,
    maxLength: maxFields,
  }).map((entries) => Object.fromEntries(entries.map(([name, node]) => [name, { node } as Field])));
}

function unwrap<A, E>(result: Either.Either<A, E>): A {
  if (Either.isLeft(result)) {
    throw new Error(`arbitrary produced an invalid declaration: ${JSON.stringify(result.left)}`);
  }
  return result.right;
}

/** A valid `Table`: generated fields, either layout, and zero or more valid single- or multi-field indexes. */
export const tableArb: fc.Arbitrary<Table> = fc.tuple(
  fieldsArb(2, 5),
  fc.constantFrom("columns", "json" as const),
).chain(
  ([fields, storage]) => {
    const scalars = Object.keys(fields).filter((k) => fields[k].node.kind !== "array");
    const indexes = scalars.length === 0 ? fc.constant([]) : fc.uniqueArray(
      fc.tuple(identifierArb, fc.uniqueArray(fc.constantFrom(...scalars), { minLength: 1, maxLength: 3 })),
      { selector: ([name]) => name, maxLength: 2 },
    );
    return indexes.map((idx) => {
      let table: Table = unwrap(defineTable(fields)).storeAs(storage as "columns" | "json");
      for (const [name, on] of idx) table = unwrap(table.addIndex(name, on));
      return table;
    });
  },
);

/** A valid `Schema`: 0..3 tables under distinct, unreserved names. */
export const schemaArb: fc.Arbitrary<Schema> = fc.uniqueArray(fc.tuple(publicNameArb, tableArb), {
  selector: ([name]) => name,
  minLength: 0,
  maxLength: 3,
}).map((entries) => unwrap(defineSchema(Object.fromEntries(entries))));
