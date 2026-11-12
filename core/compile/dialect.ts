/**
 * The two SQL dialects this repo generates, and the two primitives that put untrusted names and
 * strings into generated SQL safely.
 *
 * PURE, per the `core/` rule in `docs/adr/0001-effect-port.md`: `effect`, `../errors.ts` and
 * `../schema.ts` only. Nothing in `core/compile/` raises, so `quote` returns an `Either` rather than rejecting
 * a bad identifier the way the pre-port compiler (the Deno platform's `compiler.ts`) did. Callers that
 * need a plain `Error` render it from the tagged error; there are no throwing wrappers.
 *
 * `quote` cannot be both total and identifier-checking, and dropping the check is exactly the
 * injection bug this module exists to make impossible. So `literal` is total and `quote` is an
 * `Either`.
 */
import { Either } from "effect";
import type { SchemaError } from "../errors.ts";
import { identifier } from "../schema.ts";

/** The engines: SQLite is the writer, DuckDB the reader. They differ in JSON access and binding. */
export type Dialect = "sqlite" | "duckdb";

/** What a prepared statement can carry. Booleans are encoded as `0`/`1`, arrays as JSON text. */
export type Bind = string | number | null;

/** A parameterized statement. `values` is positional, in the order the placeholders appear. */
export type SQL = { text: string; values: Bind[] };

/** One row as the engines hand it back: column name to driver-native value (BIGINT may be a bigint). */
export type Row = Record<string, unknown>;

/**
 * Quotes an identifier for interpolation into SQL. The identifier is *checked*, not escaped: only
 * `/^[A-Za-z_][A-Za-z0-9_]*$/` gets through (`core/schema.ts`'s `identifier`), so there is no
 * quoted-quote case to get wrong. Table and field names are schema-declared and already validated
 * at `defineTable`/`defineSchema` time; this is the second line of defence.
 */
export function quote(name: string): Either.Either<string, SchemaError> {
  return Either.map(identifier(name), (checked) => `"${checked}"`);
}

/** A single-quoted SQL string literal. Only ever applied to schema-derived text, never to user data. */
export function literal(s: string): string {
  return `'${s.replaceAll("'", "''")}'`;
}

/**
 * The placeholder for the `index`-th bind value (1-based): SQLite takes positional `?`, DuckDB
 * named `$pN`. Values always travel out-of-band in `SQL.values`.
 */
export function placeholder(dialect: Dialect, index: number): string {
  return dialect === "sqlite" ? "?" : `$p${index}`;
}
