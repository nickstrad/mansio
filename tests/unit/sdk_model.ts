/**
 * A pure in-memory `/v1/ops` server for the SDK's tests (IR-3): enough of `ir/spec/v1.md` §5–§8 to run
 * what the recording executor sends — `get`, `select` (eq/gt filters, order with the `_id` tie-break,
 * nulls first ascending, `limit`, `count`, `countBy`), `insert` with a client id, `patch`, `delete`,
 * `assert` with `expect.version`/`exists` or, without an id, `expect.unchangedSince`, the commit sequence,
 * request-id duplicates — atomically per request. No validation:
 * the real executors own that (the vectors pin it). Written independently of `sdk/overlay.ts`, so the
 * overlay is checked against a second implementation of the select rules.
 *
 * `fetch` serves it over a fake HTTP surface, with `faults` to inject transport and server failures.
 */
import type { Op, Request, SelectOp } from "../../core/ir/types.ts";

export type Doc = Record<string, unknown> & { _id: string; _creationTime: number; _version: number };
type Wire = { status: number; body: unknown; headers?: Record<string, string> };

class Failure {
  constructor(readonly status: number, readonly tag: string, readonly op: number, readonly fields: object) {}
}

function cmp(a: unknown, b: unknown): number {
  const an = a === null || a === undefined, bn = b === null || b === undefined;
  if (an || bn) return an && bn ? 0 : an ? -1 : 1;
  if (typeof a === "boolean") a = Number(a);
  if (typeof b === "boolean") b = Number(b);
  return (a as number | string) < (b as number | string)
    ? -1
    : (a as number | string) > (b as number | string)
    ? 1
    : 0;
}

/** The select's answer over `docs` (ASCII text only: JS string order is the code point order there). */
export function evalSelect(docs: Iterable<Doc>, op: SelectOp): unknown {
  const rows = [...docs].filter((d) =>
    op.filters.every((f) =>
      f.op === "eq" ? (d[f.field] ?? null) === f.value : typeof d[f.field] === "number" &&
        (d[f.field] as number) > (f.value as number)
    )
  );
  if (op.aggregate?.kind === "count") return rows.length;
  if (op.aggregate?.kind === "countBy") {
    const field = op.aggregate.field;
    const counts = new Map<unknown, number>();
    for (const d of rows) counts.set(d[field] ?? null, (counts.get(d[field] ?? null) ?? 0) + 1);
    return [...counts.entries()].sort(([a], [b]) => cmp(a, b)).slice(0, op.limit).map(([value, count]) => ({
      value,
      count,
    }));
  }
  rows.sort((a, b) => cmp(a._id, b._id));
  if (op.order) {
    const { field, direction } = op.order;
    rows.sort((a, b) => (direction === "asc" ? 1 : -1) * cmp(a[field], b[field]) || cmp(a._id, b._id));
  }
  return rows.slice(0, op.limit);
}

export class Model {
  tables = new Map<string, Map<string, Doc>>();
  applied = new Map<string, unknown[]>();
  clock = 1000;
  /** The commit sequence (spec §6.5): mutations applied that changed a row. */
  seq = 0;
  /** Each table's last change, as a sequence (spec §5.5). */
  changes = new Map<string, number>();
  private touched = new Set<string>();
  /** Every request received, in order. */
  log: Request[] = [];

  table(name: string): Map<string, Doc> {
    let t = this.tables.get(name);
    if (!t) this.tables.set(name, t = new Map());
    return t;
  }

  seed(table: string, docs: Doc[]) {
    for (const d of docs) this.table(table).set(d._id, structuredClone(d));
  }

  clone(): Model {
    const m = new Model();
    m.tables = new Map(
      [...this.tables].map(([k, t]) => [k, new Map([...t].map(([id, d]) => [id, structuredClone(d)]))]),
    );
    m.clock = this.clock;
    m.seq = this.seq;
    m.changes = new Map(this.changes);
    return m;
  }

  /** Runs one request; answers the wire form (spec §8). */
  exec(request: Request): Wire {
    this.log.push(request);
    if (request.kind === "mutation" && this.applied.has(request.requestId!)) {
      return { status: 200, body: { result: this.applied.get(request.requestId!), duplicate: true } };
    }
    const work = this.clone();
    const result: unknown[] = [];
    try {
      request.ops.forEach((op, i) => result.push(work.step(op, i)));
    } catch (e) {
      if (!(e instanceof Failure)) throw e;
      const fields = e.tag === "ConflictError"
        ? { ...e.fields, ...work.witness(request.ops, e.op) }
        : e.fields;
      return {
        status: e.status,
        body: { error: { tag: e.tag, message: e.tag, op: e.op, fields } },
      };
    }
    if (request.kind === "mutation") {
      this.tables = work.tables;
      if (work.touched.size > 0) this.seq++;
      for (const table of work.touched) this.changes.set(table, this.seq);
      this.applied.set(request.requestId!, result);
    }
    return { status: 200, body: { result, seq: this.seq } };
  }

  /**
   * A conflict's `seq` and `current` (spec §8.1): the state behind the failing operation's precondition and
   * behind every later assert that fails too, in the state the failing operation saw. No size cap.
   */
  private witness(ops: Op[], at: number): { seq: number; current: unknown[] } {
    const current: unknown[] = [];
    ops.forEach((op, i) => {
      if (i < at || (i > at && op.op !== "assert")) return;
      if (i > at) {
        try {
          this.step(op, i);
          return; // it holds
        } catch (e) {
          if (!(e instanceof Failure)) throw e;
        }
      }
      if (op.op === "assert" && !("id" in op)) {
        const changed = this.touched.has(op.table) ? this.seq + 1 : this.changes.get(op.table) ?? 0;
        current.push({ op: i, table: op.table, changed });
      } else {
        const id = (op as { id: string }).id;
        current.push({ op: i, table: op.table, id, doc: this.table(op.table).get(id) ?? null });
      }
    });
    return { seq: this.seq, current };
  }

  private step(op: Op, i: number): unknown {
    const t = this.table(op.table);
    switch (op.op) {
      case "select":
        return evalSelect(t.values(), op);
      case "get":
        return t.get(op.id as string) ?? null;
      case "insert": {
        const id = op.id!;
        if (t.has(id)) {
          throw new Failure(409, "ConflictError", i, {
            table: op.table,
            id,
            check: "exists",
            expected: false,
            actual: true,
          });
        }
        this.touched.add(op.table);
        t.set(id, { ...op.value, _id: id, _creationTime: this.clock, _version: 1 } as Doc);
        return id;
      }
      case "patch":
      case "delete":
      case "assert": {
        if (!("id" in op)) {
          const changed = this.touched.has(op.table) ? this.seq + 1 : this.changes.get(op.table) ?? 0;
          if (changed > op.expect.unchangedSince) {
            throw new Failure(409, "ConflictError", i, {
              table: op.table,
              check: "unchanged",
              expected: op.expect.unchangedSince,
              actual: changed,
            });
          }
          return null;
        }
        const id = op.id as string;
        const doc = t.get(id) ?? null;
        const expect = op.expect;
        if (expect) {
          const fields = { table: op.table, id };
          const mustExist = expect.exists ?? (expect.version !== undefined ? true : undefined);
          if (mustExist !== undefined && mustExist !== (doc !== null)) {
            throw new Failure(409, "ConflictError", i, {
              ...fields,
              check: "exists",
              expected: mustExist,
              actual: doc !== null,
            });
          }
          if (doc && expect.version !== undefined && doc._version !== expect.version) {
            throw new Failure(409, "ConflictError", i, {
              ...fields,
              check: "version",
              expected: expect.version,
              actual: doc._version,
            });
          }
        }
        if (op.op === "assert") return null;
        if (op.op === "delete") {
          if (doc !== null) this.touched.add(op.table);
          t.delete(id);
          return { deleted: doc !== null };
        }
        if (!doc) throw new Failure(404, "NotFoundError", i, { table: op.table, id });
        this.touched.add(op.table);
        t.set(id, { ...doc, ...op.value, _version: doc._version + 1 });
        return { _version: doc._version + 1 };
      }
    }
  }
}

/** What a fault does to one request. */
export type Fault =
  | { kind: "shed"; retryAfter?: string } // 503 shed, nothing ran
  | { kind: "aborted" } // 409 aborted, nothing ran
  | { kind: "expired" } // 504 expired, ambiguous, nothing ran
  | { kind: "lost" } // the request ran, then the connection dropped (no answer)
  | { kind: "refused" } // no connection: nothing ran
  | { kind: "before"; run: () => void }; // run something (a concurrent writer), then serve normally

/**
 * A `fetch` serving `model` at any `…/v1/ops` URL. `fault(request, n)` (n = 0-based count of requests so
 * far) may inject a failure.
 */
export function modelFetch(
  model: Model,
  fault: (request: Request, n: number) => Fault | undefined = () => undefined,
  seen: { headers: Headers[] } = { headers: [] },
): typeof fetch {
  let n = 0;
  return (async (_url: string | URL | globalThis.Request, init?: RequestInit) => {
    await Promise.resolve(); // yield, so concurrent clients interleave
    const request = JSON.parse(String(init!.body)) as Request;
    seen.headers.push(new Headers(init!.headers));
    const f = fault(request, n++);
    const json = (w: Wire) =>
      new Response(JSON.stringify(w.body), { status: w.status, headers: w.headers ?? {} });
    switch (f?.kind) {
      case "shed":
        return json({
          status: 503,
          body: { error: { tag: "shed", message: "shed" } },
          headers: f.retryAfter ? { "retry-after": f.retryAfter } : {},
        });
      case "aborted":
        return json({ status: 409, body: { error: { tag: "aborted", message: "aborted" } } });
      case "expired":
        return json({
          status: 504,
          body: { error: { tag: "expired", message: "expired", fields: { ambiguous: true } } },
        });
      case "refused":
        throw new TypeError("connection refused");
      case "lost":
        model.exec(request);
        throw new TypeError("connection reset");
      case "before":
        f.run();
    }
    return json(model.exec(request));
  }) as typeof fetch;
}
