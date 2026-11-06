/**
 * `core/ir/`: the strict parser agrees with `JSON.parse` wherever it accepts, the canonical encoding
 * is RFC 8785-shaped, and the wire error form hides 5xx detail. The protocol's behaviour as a whole is
 * `ir_vectors_test.ts`'s job; these pin the pieces the vectors cannot see.
 */
import assert from "node:assert/strict";
import fc from "fast-check";
import { Either } from "effect";
import { StorageError } from "../../core/errors.ts";
import { IrFailure, toWireError } from "../../core/ir/errors.ts";
import { canonical, parseStrict } from "../../core/ir/json.ts";

Deno.test("parseStrict equals JSON.parse on JSON without duplicate members", () => {
  fc.assert(
    fc.property(fc.jsonValue(), (value) => {
      const text = JSON.stringify(value);
      const parsed = parseStrict(text);
      assert.ok(Either.isRight(parsed), text);
      assert.deepEqual(parsed.right, JSON.parse(text));
    }),
    { numRuns: 300 },
  );
});

Deno.test("canonical round-trips and is stable", () => {
  fc.assert(
    fc.property(fc.jsonValue(), (value) => {
      const text = canonical(value);
      assert.deepEqual(JSON.parse(text), JSON.parse(JSON.stringify(value)));
      assert.equal(canonical(JSON.parse(text)), text);
    }),
    { numRuns: 300 },
  );
});

Deno.test("canonical: sorted members, no whitespace, ECMAScript numbers", () => {
  assert.equal(
    canonical({ b: [1, 1e21, 0.1, -0], a: { "é": null, "Z": "\u0000\n" }, c: undefined }),
    '{"a":{"Z":"\\u0000\\n","é":null},"b":[1,1e+21,0.1,0]}',
  );
  // UTF-16 code-unit order: U+FFFF sorts after an astral character's high surrogate.
  assert.equal(canonical({ "￿": 1, "\u{1F600}": 2 }), '{"\u{1F600}":2,"￿":1}');
});

Deno.test("parseStrict refusals", () => {
  for (
    const [text, path] of [
      ['{"a":1,"a":2}', "a"],
      ['{"a":[{"b":1,"b":1}]}', "a[0].b"],
      ['"\\udc00"', "body"],
      ["1e999", "body"],
      ["[1,]", "[1]"],
      ["01", "body"],
      ['{"a":1} 2', "body"],
      ["[".repeat(66) + "]".repeat(66), "[0]".repeat(65)],
    ] as const
  ) {
    const parsed = parseStrict(text);
    assert.ok(Either.isLeft(parsed), text);
    assert.equal(parsed.left.path, path, text);
  }
});

Deno.test("toWireError: 5xx carries only the tag", () => {
  const wire = toWireError(
    new IrFailure({ cause: new StorageError({ operation: "insert", detail: "disk /x full" }), op: 2 }),
  );
  assert.deepEqual(wire, {
    status: 500,
    body: { error: { tag: "StorageError", message: "StorageError", op: 2 } },
  });
});
