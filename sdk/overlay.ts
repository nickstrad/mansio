/**
 * Read-your-writes for `find` inside a mutation handler (IR-3): a select's server answer, corrected for
 * the writes the handler has buffered but not yet sent.
 *
 * PURE. The recording executor (`sdk/recording.ts`) asks the server for the select with its limit widened
 * by the number of documents the handler touched in that table, plus a `get` of each touched document in
 * the same query request (one snapshot). This module then removes the touched documents' server state from
 * the answer, adds their local state where it matches, and re-applies the select's order and limit. The
 * result is exactly what the select would answer on a database holding the server's snapshot with the
 * handler's writes applied: at most `touched` server rows are displaced, so the widened limit always covers
 * the first `limit` untouched rows (and, for `countBy`, the first `limit` untouched groups).
 *
 * The rules are the compiler's (`ir/spec/v1.md` §5.1): `eq null` matches null; `gt` matches a non-null
 * number above the value; without `order`, `_id` ascending; with `order`, nulls first ascending and last
 * descending, then `_id` ascending; `count` ignores `limit`; `countBy` groups by value, null first, and
 * `limit` caps the groups. Strings compare by code point, which is SQLite's BINARY order over UTF-8.
 */
import { MAX_LIMIT } from "../core/plan.ts";
import type { SelectOp } from "../core/ir/types.ts";

export type Doc = Record<string, unknown> & { _id: string };

/** Whether `doc` satisfies every filter of `op`. */
export function matches(op: SelectOp, doc: Doc): boolean {
  return op.filters.every((filter) => {
    const value = doc[filter.field] ?? null;
    if (filter.op === "eq") return value === filter.value;
    return value !== null && typeof value === "number" && typeof filter.value === "number" &&
      value > filter.value;
  });
}

/** SQLite's order over the stored scalars of one field: numbers, booleans (0/1) and text by code point. */
export function compareValues(a: unknown, b: unknown): number {
  if (typeof a === "boolean") a = a ? 1 : 0;
  if (typeof b === "boolean") b = b ? 1 : 0;
  if (typeof a === "number" && typeof b === "number") return a < b ? -1 : a > b ? 1 : 0;
  if (typeof a === "string" && typeof b === "string") {
    const x = [...a], y = [...b];
    for (let i = 0; i < Math.min(x.length, y.length); i++) {
      const d = x[i].codePointAt(0)! - y[i].codePointAt(0)!;
      if (d !== 0) return d < 0 ? -1 : 1;
    }
    return x.length - y.length < 0 ? -1 : x.length > y.length ? 1 : 0;
  }
  return 0;
}

/** Nulls first (the ascending position); `compareValues` otherwise. */
function compareNullable(a: unknown, b: unknown): number {
  const an = a === null || a === undefined, bn = b === null || b === undefined;
  if (an || bn) return an && bn ? 0 : an ? -1 : 1;
  return compareValues(a, b);
}

/** The select's row order: its `order` (nulls first asc, last desc), then `_id` ascending. */
export function compareRows(op: SelectOp, a: Doc, b: Doc): number {
  if (op.order) {
    const c = compareNullable(a[op.order.field], b[op.order.field]);
    if (c !== 0) return op.order.direction === "asc" ? c : -c;
  }
  return compareValues(a._id, b._id);
}

/** The limit to ask the server for when `touched` documents of the table have local state. */
export function widenedLimit(op: SelectOp, touched: number): number {
  return Math.min(MAX_LIMIT, op.limit + touched);
}

/**
 * The corrected answer of `op`. `server` is the server's answer to `op` with its limit widened by
 * `widenedLimit`; `before` holds the server's current state of every touched document (null when
 * missing) and `after` its local state (null when deleted or still missing), keyed by `_id`.
 */
export function overlay(
  op: SelectOp,
  server: unknown,
  before: ReadonlyMap<string, Doc | null>,
  after: ReadonlyMap<string, Doc | null>,
): unknown {
  const local = [...after.values()].filter((doc): doc is Doc => doc !== null && matches(op, doc));
  const touchedMatching = [...before.values()].filter((doc): doc is Doc => doc !== null && matches(op, doc));
  if (op.aggregate?.kind === "count") return (server as number) - touchedMatching.length + local.length;
  if (op.aggregate?.kind === "countBy") {
    const field = op.aggregate.field;
    const groups = new Map<unknown, number>();
    for (const group of server as { value: unknown; count: number }[]) groups.set(group.value, group.count);
    const bump = (value: unknown, by: number) => groups.set(value, (groups.get(value) ?? 0) + by);
    for (const doc of touchedMatching) bump(doc[field] ?? null, -1);
    for (const doc of local) bump(doc[field] ?? null, 1);
    return [...groups].filter(([, count]) => count > 0)
      .sort(([a], [b]) => compareNullable(a, b))
      .slice(0, op.limit)
      .map(([value, count]) => ({ value, count }));
  }
  const rows = (server as Doc[]).filter((doc) => !after.has(doc._id));
  return [...rows, ...local].sort((a, b) => compareRows(op, a, b)).slice(0, op.limit);
}
