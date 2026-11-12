/**
 * Strict JSON for the IR wire (`intermediate-representation/spec/v1.md` §3): a parser that rejects what `JSON.parse` silently
 * accepts, and the canonical encoding.
 *
 * PURE: `effect` and `../errors.ts` only; nothing in here raises. A rejection is a `SchemaError`
 * whose `path` names where in the document the parser stopped (`ops[2].table` for a duplicate member,
 * `body` when the text is not JSON at all).
 *
 * Why not `JSON.parse`: the Go backend decodes the same bytes, and the two languages must agree on what
 * a request means. `JSON.parse` keeps the last of two duplicate members, turns `1e400` into `Infinity`
 * and accepts lone UTF-16 surrogates that Go would replace with U+FFFD. This parser refuses all three,
 * and otherwise produces exactly the value `JSON.parse` would (numbers through `Number(text)`).
 */
import { Either } from "effect";
import { SchemaError } from "../errors.ts";

/** Nesting deeper than this is refused: no legal request needs it, and it bounds recursion. */
export const MAX_DEPTH = 64;

const NUMBER = /-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/y;
const WHITESPACE = /[ \t\n\r]*/y;
const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;

/** One parse failure: where (path) and why (expected). */
class Stop {
  constructor(readonly path: string, readonly expected: string) {}
}

/** `path` extended by an object member or an array index, in `core/schema.ts`'s path style. */
export function member(path: string, key: string): string {
  return path === "" ? key : `${path}.${key}`;
}
export function element(path: string, index: number): string {
  return `${path}[${index}]`;
}

/**
 * Parses `text` as one JSON value. Fails `SchemaError` (`type-mismatch`) on malformed text, a
 * duplicate member name, a number outside the double range, a string holding a lone surrogate, or
 * nesting deeper than `MAX_DEPTH`.
 */
export function parseStrict(text: string): Either.Either<unknown, SchemaError> {
  let at = 0;
  const skip = () => {
    WHITESPACE.lastIndex = at;
    WHITESPACE.exec(text);
    at = WHITESPACE.lastIndex;
  };
  /** A string at `path`; a member name passes `key`, so a refused name is reported at its own path. */
  const string = (path: string, key = false): string | Stop => {
    // `at` is on the opening quote. Scan to the closing quote, then let JSON.parse decode escapes:
    // the scan has already checked the grammar it relies on.
    let end = at + 1;
    for (;;) {
      if (end >= text.length) return new Stop(path, "JSON");
      const c = text.charCodeAt(end);
      if (c === 0x22) break;
      if (c < 0x20) return new Stop(path, "JSON");
      if (c === 0x5c) {
        const e = text[end + 1];
        if (e === "u") {
          if (!/^[0-9a-fA-F]{4}$/.test(text.slice(end + 2, end + 6))) return new Stop(path, "JSON");
          end += 6;
        } else if (e !== undefined && '"\\/bfnrt'.includes(e)) end += 2;
        else return new Stop(path, "JSON");
      } else end++;
    }
    const decoded = JSON.parse(text.slice(at, end + 1)) as string;
    if (LONE_SURROGATE.test(decoded)) {
      return new Stop(key ? member(path, decoded) : path, "well-formed Unicode text");
    }
    at = end + 1;
    return decoded;
  };
  /** A value at `path`, inside `depth` open containers (the top level is depth 0). */
  const value = (path: string, depth: number): unknown | Stop => {
    skip();
    const c = text[at];
    // At most MAX_DEPTH containers deep: a container opened inside MAX_DEPTH others is refused.
    if ((c === "{" || c === "[") && depth >= MAX_DEPTH) {
      return new Stop(path, `nesting at most ${MAX_DEPTH} arrays/objects deep`);
    }
    if (c === "{") {
      at++;
      const out: Record<string, unknown> = {};
      skip();
      if (text[at] === "}") {
        at++;
        return out;
      }
      for (;;) {
        skip();
        if (text[at] !== '"') return new Stop(path, "JSON");
        const key = string(path, true);
        if (key instanceof Stop) return key;
        const at_ = member(path, key);
        if (Object.hasOwn(out, key)) return new Stop(at_, "unique member name");
        skip();
        if (text[at] !== ":") return new Stop(path, "JSON");
        at++;
        const item = value(at_, depth + 1);
        if (item instanceof Stop) return item;
        // defineProperty, not assignment: a member named `__proto__` stays an own data property.
        Object.defineProperty(out, key, {
          value: item,
          enumerable: true,
          writable: true,
          configurable: true,
        });
        skip();
        if (text[at] === ",") {
          at++;
          continue;
        }
        if (text[at] === "}") {
          at++;
          return out;
        }
        return new Stop(path, "JSON");
      }
    }
    if (c === "[") {
      at++;
      const out: unknown[] = [];
      skip();
      if (text[at] === "]") {
        at++;
        return out;
      }
      for (;;) {
        const item = value(element(path, out.length), depth + 1);
        if (item instanceof Stop) return item;
        out.push(item);
        skip();
        if (text[at] === ",") {
          at++;
          continue;
        }
        if (text[at] === "]") {
          at++;
          return out;
        }
        return new Stop(path, "JSON");
      }
    }
    if (c === '"') return string(path);
    for (const [word, literal] of [["true", true], ["false", false], ["null", null]] as const) {
      if (text.startsWith(word, at)) {
        at += word.length;
        return literal;
      }
    }
    NUMBER.lastIndex = at;
    const match = NUMBER.exec(text);
    if (match === null) return new Stop(path, "JSON");
    at = NUMBER.lastIndex;
    const n = Number(match[0]);
    if (!Number.isFinite(n)) return new Stop(path, "finite number");
    return n;
  };
  const result = value("", 0);
  skip();
  const stop = result instanceof Stop ? result : at < text.length ? new Stop("", "JSON") : undefined;
  if (stop !== undefined) {
    return Either.left(
      new SchemaError({
        reason: "type-mismatch",
        path: stop.path === "" ? "body" : stop.path,
        expected: stop.expected,
      }),
    );
  }
  return Either.right(result);
}

/**
 * The canonical encoding (RFC 8785, JSON Canonicalization Scheme): no whitespace, object members
 * sorted by UTF-16 code units, numbers and strings exactly as ECMAScript `JSON.stringify` writes them,
 * `undefined` members dropped. Two values that mean the same thing encode to the same text, which is
 * what a stored duplicate result and an SDK "same op list" comparison rely on.
 */
export function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map((item) => canonical(item ?? null)).join(",")}]`;
  if (value !== null && typeof value === "object") {
    const record = value as Record<string, unknown>;
    const members = Object.keys(record)
      .filter((key) => record[key] !== undefined)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonical(record[key])}`);
    return `{${members.join(",")}}`;
  }
  return JSON.stringify(value);
}
