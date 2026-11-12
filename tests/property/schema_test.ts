/**
 * Property tests for `core/schema.ts`.
 *
 * Each property is falsifiable: the generators build values from a node's structure (never by
 * calling `validate`), and the mismatch generator states the failure path it expects.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import fc from "fast-check";
import { Either } from "effect";
import { SchemaError } from "../../core/errors.ts";
import { defineSchema, defineTable, identifier, parseObject, validate } from "../../core/schema.ts";
import {
  arrayDepth,
  check,
  fieldsArb,
  identifierArb,
  invalidIdentifierArb,
  matchingValueArb,
  mismatchArb,
  nodeArb,
  schemaArb,
  SEED,
  tableArb,
  titled,
} from "./arbitraries.ts";

const ROOT = "x";

function right<A>(result: Either.Either<A, SchemaError>): A {
  if (Either.isLeft(result)) assert.fail(`expected Right, got ${JSON.stringify(result.left)}`);
  return result.right;
}
function left<A>(result: Either.Either<A, SchemaError>): SchemaError {
  if (Either.isRight(result)) assert.fail(`expected Left, got Right(${JSON.stringify(result.right)})`);
  return result.left;
}

/** Fields plus one complete valid object over them. */
const fieldsAndObject = fieldsArb(2, 5).chain((fields) =>
  fc.tuple(...Object.entries(fields).map(([k, f]) => matchingValueArb(f.node).map((v) => [k, v] as const)))
    .map((pairs) => ({ fields, object: Object.fromEntries(pairs) as Record<string, unknown> }))
);

test(titled("generators: depth bound, kinds and nullability are all exercised"), () => {
  const nodes = fc.sample(nodeArb(2), { numRuns: 400, seed: SEED });
  assert.ok(nodes.every((n) => arrayDepth(n) <= 2), "array nesting must stay <= 2");
  assert.ok(nodes.some((n) => arrayDepth(n) === 2), "depth 2 must be reachable");
  for (const kind of ["string", "number", "integer", "boolean", "enum", "array"]) {
    assert.ok(nodes.some((n) => n.kind === kind), `kind ${kind} never generated`);
  }
  assert.ok(nodes.some((n) => n.nullable) && nodes.some((n) => !n.nullable));
  assert.ok(nodes.some((n) => n.kind === "array" && n.length !== undefined), "declared array lengths");
  // Mismatches must reach into arrays, not just top-level type errors.
  const mismatches = fc.sample(nodeArb(2).chain((n) => mismatchArb(n, ROOT)), { numRuns: 400, seed: SEED });
  assert.ok(mismatches.some((m) => m.path.includes("][")), "nested element paths never generated");
  assert.ok(mismatches.some((m) => /^x\[\d+\]$/.test(m.path)), "element paths never generated");
});

test(titled("a value generated to match a node always validates, and comes back unchanged"), () => {
  check(
    fc.property(
      nodeArb(2).chain((node) => matchingValueArb(node).map((value) => ({ node, value }))),
      ({ node, value }) => {
        assert.deepStrictEqual(right(validate(node, value, ROOT)), value);
      },
    ),
  );
});

test(titled("a value generated not to match always fails with a SchemaError at the offending path"), () => {
  check(
    fc.property(
      nodeArb(2).chain((node) => mismatchArb(node, ROOT).map((m) => ({ node, ...m }))),
      ({ node, value, path }) => {
        const error = left(validate(node, value, ROOT));
        assert.ok(error instanceof SchemaError);
        assert.equal(error._tag, "SchemaError");
        assert.equal(error.reason, "type-mismatch");
        assert.equal(error.path, path);
      },
    ),
  );
});

test(titled("a null is accepted exactly when the node is nullable"), () => {
  check(fc.property(nodeArb(2), (node) => {
    const result = validate(node, null, ROOT);
    assert.equal(Either.isRight(result), node.nullable === true);
  }));
});

test(titled("parseObject is total: Left or Right for arbitrary input, never a throw"), () => {
  const exotic = fc.anything({
    withBigInt: true,
    withDate: true,
    withMap: true,
    withSet: true,
    withNullPrototype: true,
    withSparseArray: true,
    withTypedArray: true,
    withObjectString: true,
  });
  const jsonText = fc.json().map((text) => JSON.parse(text) as unknown);
  check(fc.property(
    fieldsArb(2, 5),
    fc.oneof(fc.jsonValue(), jsonText, exotic, fc.constantFrom(undefined, null, NaN, Infinity, "", 0)),
    fc.boolean(),
    (fields, input, partial) => {
      const result = parseObject(fields, input, partial);
      assert.ok(Either.isEither(result));
      if (Either.isLeft(result)) assert.ok(result.left instanceof SchemaError);
      else assert.ok(typeof result.right === "object" && result.right !== null);
    },
  ));
  // Objects over the declared keys with arbitrary JSON values reach the per-field validators.
  check(fc.property(
    fieldsArb(2, 5).chain((fields) =>
      fc.tuple(
        ...Object.keys(fields).map((k) =>
          fc.option(fc.jsonValue(), { nil: undefined }).map((v) => [k, v] as const)
        ),
      )
        .map((pairs) => ({ fields, input: Object.fromEntries(pairs.filter(([, v]) => v !== undefined)) }))
    ),
    fc.boolean(),
    ({ fields, input }, partial) => {
      const result = parseObject(fields, input, partial);
      assert.ok(Either.isRight(result) || result.left instanceof SchemaError);
    },
  ));
});

test(titled("parseObject accepts a complete valid object and returns it unchanged"), () => {
  check(fc.property(fieldsAndObject, ({ fields, object }) => {
    assert.deepStrictEqual(right(parseObject(fields, object, false)), object);
    assert.deepStrictEqual(right(parseObject(fields, object, true)), object);
  }));
});

test(titled("partial mode accepts exactly the subsets of a valid object"), () => {
  const withSubset = fieldsAndObject.chain(({ fields, object }) =>
    fc.subarray(Object.keys(object)).map((keys) => ({
      fields,
      object,
      keys,
      subset: Object.fromEntries(keys.map((k) => [k, object[k]])),
    }))
  );
  check(fc.property(withSubset, ({ fields, object, keys, subset }) => {
    // Every subset passes in partial mode and is returned as-is (absent keys stay absent).
    assert.deepStrictEqual(right(parseObject(fields, subset, true)), subset);
    // Without partial, only the complete object passes; a missing key fails at that key.
    const strict = parseObject(fields, subset, false);
    const missing = Object.keys(fields).filter((k) => !keys.includes(k));
    if (missing.length === 0) {
      assert.deepStrictEqual(right(strict), object);
    } else {
      assert.equal(left(strict).path, missing[0], "first declared field that is absent");
    }
  }));
});

test(titled("partial mode still rejects unknown keys and bad values at the offending path"), () => {
  const bad = fieldsAndObject.filter(({ fields }) => Object.keys(fields).length > 0).chain((
    { fields, object },
  ) =>
    fc.constantFrom(...Object.keys(fields)).chain((key) =>
      mismatchArb(fields[key].node, key).map((m) => ({ fields, object, key, mismatch: m }))
    )
  );
  check(fc.property(bad, ({ fields, object, key, mismatch }) => {
    const patch = { [key]: mismatch.value };
    assert.equal(left(parseObject(fields, patch, true)).path, mismatch.path);
    assert.equal(left(parseObject(fields, { ...object, [key]: mismatch.value }, false)).path, mismatch.path);
  }));
  check(fc.property(fieldsAndObject, identifierArb, fc.jsonValue(), ({ fields, object }, extra, value) => {
    fc.pre(!Object.hasOwn(fields, extra));
    for (const partial of [true, false]) {
      const error = left(parseObject(fields, { ...object, [extra]: value }, partial));
      assert.equal(error.reason, "unknown-field");
      assert.equal(error.path, extra);
    }
  }));
});

test(titled("validation is idempotent: validating the output yields the same output"), () => {
  const anyValue = nodeArb(2).chain((node) =>
    fc.oneof(
      matchingValueArb(node).map((value) => ({ node, value })),
      mismatchArb(node, ROOT).map((m) => ({ node, value: m.value })),
    )
  );
  check(fc.property(anyValue, ({ node, value }) => {
    const first = validate(node, value, ROOT);
    if (Either.isRight(first)) {
      const second = validate(node, first.right, ROOT);
      assert.deepStrictEqual(right(second), first.right);
      assert.deepStrictEqual(first.right, value, "validation returns the value, not a normalised variant");
    } else {
      const again = left(validate(node, value, ROOT));
      assert.equal(again.path, first.left.path);
      assert.equal(again.reason, first.left.reason);
    }
  }));
});

test(
  titled("identifier accepts exactly valid names and rejects every spliced or injection-shaped one"),
  () => {
    check(fc.property(identifierArb, (name) => {
      assert.equal(right(identifier(name)), name);
    }));
    check(fc.property(invalidIdentifierArb, (name) => {
      const error = left(identifier(name));
      assert.equal(error.reason, "invalid-identifier");
      assert.equal(error.path, name);
    }));
  },
);

test(
  titled("defineTable / defineSchema accept generated declarations and reject bad or reserved names"),
  () => {
    check(fc.property(fieldsArb(2, 5), (fields) => {
      assert.equal(right(defineTable(fields)).fields, fields);
    }));
    check(fc.property(fieldsArb(1, 3), invalidIdentifierArb, (fields, name) => {
      const error = left(defineTable({ ...fields, [name]: { node: { kind: "string" } } }));
      assert.equal(error.reason, "invalid-identifier");
      assert.equal(error.path, name);
    }));
    check(fc.property(fieldsArb(1, 3), identifierArb, (fields, rest) => {
      const error = left(defineTable({ ...fields, [`_${rest}`]: { node: { kind: "string" } } }));
      assert.equal(error.reason, "reserved-field");
    }));
    check(
      fc.property(tableArb, fc.constantFrom("_t", "sqlite_t", "SQLITE_master", "Sqlite_x"), (table, name) => {
        assert.equal(left(defineSchema({ [name]: table })).reason, "reserved-table");
      }),
    );
    check(fc.property(schemaArb, (schema) => {
      assert.deepStrictEqual(right(defineSchema(schema)), schema);
      for (const table of Object.values(schema)) {
        for (const index of table.indexes) {
          assert.ok(
            index.fields.every((f) =>
              Object.hasOwn(table.fields, f) && table.fields[f].node.kind !== "array"
            ),
          );
        }
      }
    }));
  },
);

test(titled("addIndex rejects unknown fields, array fields, empty lists and duplicate names"), () => {
  check(fc.property(tableArb, identifierArb, (table, name) => {
    const fieldNames = Object.keys(table.fields);
    assert.equal(left(table.addIndex(name, [])).reason, "empty-index");
    const unknown = fieldNames.includes("missing_field") ? "missing_field_2" : "missing_field";
    assert.equal(left(table.addIndex(name, [unknown])).reason, "unindexable-field");
    for (const f of fieldNames.filter((k) => table.fields[k].node.kind === "array")) {
      assert.equal(left(table.addIndex(name, [f])).reason, "unindexable-field");
    }
    for (const index of table.indexes) {
      const scalar = fieldNames.find((k) => table.fields[k].node.kind !== "array");
      if (scalar) assert.equal(left(table.addIndex(index.name, [scalar])).reason, "duplicate-index");
    }
  }));
});
