/**
 * The recording executor (IR-3, `docs/plans/ir-protocol.md` §3): runs a handler in the caller's process
 * against the remote database, and turns what it did into one IR request.
 *
 * - **Reads go out at once** as query requests (`Reads`), one per `get`/`find` terminal, and are cached
 *   per handler run: a second `get` of the same document answers from the cache.
 * - **Writes are buffered.** `insert` draws a client id (UUIDv7) and answers it at once; `patch` and
 *   `delete` answer nothing. Later reads in the same run see them (read-your-writes): a `get` answers the
 *   local state, and a `find` corrects the server's answer with `sdk/overlay.ts` (one query request:
 *   the widened select plus a `get` of each document the run patched or deleted).
 * - **Preconditions are derived from the reads.** Every document a handler `get` observed on the server
 *   becomes an `assert` at the front of the mutation: `{version: v}` if it existed, `{exists: false}` if
 *   it did not. A `find` is unguarded in v1 (a range assert is v1.1): neither its answer nor the documents
 *   it returns are asserted, so a decision that depends on what a `find` saw ("no row matches") is not
 *   protected. Use `get` for documents a decision depends on.
 *
 * Asserts go first, before every write, so a conflict on op index `i < asserts` is always one of ours
 * (derived) and the client may re-run the handler; an app-raised `ConflictError` never reaches the
 * server at all (the handler fails locally). Inserts carry the spec's implicit `exists: false` on their
 * client id; a collision there is also safe to re-run (new ids are drawn).
 *
 * Local documents: a patch increments `_version` as the server will; an insert's `_creationTime` is the
 * caller's clock, a provisional value (the server stamps its own at commit, spec §6.3), and its
 * `_version` is 1. A patch of a document the run knows is missing fails `NotFoundError` locally, exactly
 * where the serial execution would have.
 *
 * Handlers may run more than once (on a derived conflict), so they must have no effects beyond `db`.
 */
import { Effect } from "effect";
import { type DbError, NotFoundError, RuleError } from "../core/errors.ts";
import { type AssertOp, type GetOp, MAX_OPS, type Op, type SelectOp } from "../core/ir/types.ts";
import { type Equality, type Execute, Find, type Operation, type SelectPlan, type WriteDB } from "./db.ts";
import type { Body, Document, Schema } from "./schema.ts";
import type { IdSource } from "./ids.ts";
import { type Doc, overlay, widenedLimit } from "./overlay.ts";
import { MAX_LIMIT } from "../core/plan.ts";

/**
 * Sends one query request (1–64 `get`/`select` ops) and answers its results, in op order. A failure is
 * not a `DbError`: the client raises it as a defect it recognises (`sdk/client.ts`), so it reaches the
 * caller as a `ClientError` without widening the handler's error channel.
 */
export type Reads = (ops: (GetOp | SelectOp)[]) => Effect.Effect<unknown[]>;

type Write =
  | { op: "insert"; value: Record<string, unknown>; creationTime: number }
  | { op: "patch"; value: Record<string, unknown> }
  | { op: "delete" };

/** What one run knows about one document. */
interface Entry {
  readonly table: string;
  readonly id: string;
  /** The server's state when first fetched; absent until then. */
  base?: Doc | null;
  /** A handler `get` observed `base` on the server: assert it at commit. */
  guarded: boolean;
  /** Buffered writes, in order. */
  readonly writes: Write[];
}

const key = (table: string, id: string) => `${table}\u0000${id}`;

/** One handler run's recording. */
export class Recording<S extends Schema> implements WriteDB<S> {
  private readonly entries = new Map<string, Entry>();
  private readonly ops: Op[] = [];

  constructor(
    private readonly reads: Reads,
    private readonly ids: IdSource,
    private readonly now: () => number,
    /** False for a query handler: it gets the read half only (the types already forbid writes). */
    private readonly writable = true,
  ) {}

  private entry(table: string, id: string): Entry {
    let entry = this.entries.get(key(table, id));
    if (!entry) {
      entry = { table, id, guarded: false, writes: [] };
      this.entries.set(key(table, id), entry);
    }
    return entry;
  }

  /** The local state of `entry` (base plus writes), or `NotFoundError` from a patch of a missing doc. */
  private view(entry: Entry): Effect.Effect<Doc | null, NotFoundError> {
    let doc = entry.base ?? null;
    for (const write of entry.writes) {
      if (write.op === "insert") {
        doc = { ...write.value, _id: entry.id, _creationTime: write.creationTime, _version: 1 };
      } else if (write.op === "delete") doc = null;
      else if (doc === null) return Effect.fail(new NotFoundError({ table: entry.table, id: entry.id }));
      else doc = { ...doc, ...write.value, _version: (doc._version as number) + 1 };
    }
    return Effect.succeed(doc);
  }

  /** Whether `entry`'s local state is known without asking the server. */
  private known(entry: Entry): boolean {
    return entry.base !== undefined || entry.writes[0]?.op === "insert";
  }

  get<K extends keyof S & string>(table: K, id: string): Effect.Effect<Document<S, K> | null, DbError> {
    const entry = this.entry(table, id);
    return Effect.gen(this, function* () {
      if (!this.known(entry)) entry.base = (yield* this.reads([{ op: "get", table, id }]))[0] as Doc | null;
      // Guard what the server told this run, not what the run wrote itself.
      if (entry.base !== undefined && entry.writes[0]?.op !== "insert") entry.guarded = true;
      return (yield* this.view(entry)) as Document<S, K> | null;
    });
  }

  find<K extends keyof S & string>(table: K, where: Equality<Document<S, K>> = {}): Find<Document<S, K>> {
    const execute: Execute = (operation: Operation) =>
      operation.kind === "select" ? this.select(operation) : Effect.dieMessage("find executes selects only");
    return new Find(execute, {
      kind: "select",
      table,
      limit: 100,
      filters: Object.entries(where).map(([field, value]) => ({ field, op: "eq", value })),
    });
  }

  /** A select, corrected for this run's writes to its table (read-your-writes; unguarded). */
  private select(plan: SelectPlan): Effect.Effect<unknown, DbError> {
    const op: SelectOp = {
      op: "select",
      table: plan.table,
      filters: plan.filters,
      limit: plan.limit,
      ...(plan.order ? { order: plan.order } : {}),
      ...(plan.aggregate ? { aggregate: plan.aggregate } : {}),
    };
    return Effect.gen(this, function* () {
      const touched = [...this.entries.values()].filter((e) => e.table === plan.table && e.writes.length > 0);
      if (touched.length === 0) return (yield* this.reads([op]))[0];
      // Only patched or deleted documents can displace a server row; an insert's fresh id has none.
      const displaced = touched.filter((entry) => entry.writes[0].op !== "insert");
      if (1 + displaced.length > MAX_OPS) {
        return yield* new RuleError({
          rule: "ir-op-limit",
          detail: `find on ${plan.table} after writes to ${displaced.length} of its documents needs ` +
            `${1 + displaced.length} reads in one request; the IR limit is ${MAX_OPS}`,
        });
      }
      // One snapshot: the widened select and the displaced documents' current server state. A document
      // this run has not read yet takes that state as its base, so its local view is from the same snapshot.
      const requested = widenedLimit(op, displaced.length);
      const [server, ...current] = yield* this.reads([
        { ...op, limit: requested },
        ...displaced.map(({ table, id }): GetOp => ({ op: "get", table, id })),
      ]);
      displaced.forEach((entry, i) => entry.base ??= current[i] as Doc | null);
      const after = new Map<string, Doc | null>();
      for (const entry of touched) after.set(entry.id, yield* this.view(entry));
      const before = new Map(displaced.map((entry, i) => [entry.id, current[i] as Doc | null]));
      const out = overlay(op, server, requested, before, after);
      if (out) return out.value;
      return yield* new RuleError({
        rule: "find-limit",
        detail:
          `find(...).limit(${plan.limit}) on ${plan.table} after writes to ${displaced.length} of its ` +
          `documents cannot be answered exactly (limit + written documents > ${MAX_LIMIT}); lower the limit`,
      });
    });
  }

  insert<K extends keyof S & string>(table: K, value: Body<S, K>): Effect.Effect<string, DbError> {
    return Effect.sync(() => {
      this.assertWritable();
      const id = this.ids();
      const body = value as Record<string, unknown>;
      this.entry(table, id).writes.push({ op: "insert", value: body, creationTime: this.now() });
      this.ops.push({ op: "insert", table, value: body, id });
      return id;
    });
  }

  patch<K extends keyof S & string>(
    table: K,
    id: string,
    value: Partial<Body<S, K>>,
  ): Effect.Effect<void, DbError> {
    const entry = this.entry(table, id);
    return Effect.suspend(() => {
      this.assertWritable();
      // A patch of a document this run knows is missing fails here, as the server would at commit.
      const check = this.known(entry) ? this.view(entry) : Effect.succeed({} as Doc);
      return Effect.flatMap(check, (doc) => {
        if (doc === null) return Effect.fail(new NotFoundError({ table, id }));
        const body = value as Record<string, unknown>;
        entry.writes.push({ op: "patch", value: body });
        this.ops.push({ op: "patch", table, id, value: body });
        return Effect.void;
      });
    });
  }

  delete<K extends keyof S & string>(table: K, id: string): Effect.Effect<void, DbError> {
    return Effect.sync(() => {
      this.assertWritable();
      this.entry(table, id).writes.push({ op: "delete" });
      this.ops.push({ op: "delete", table, id });
    });
  }

  private assertWritable() {
    if (!this.writable) throw new TypeError("a query handler cannot write");
  }

  /** The preconditions derived from this run's reads, in first-read order. */
  preconditions(): AssertOp[] {
    const out: AssertOp[] = [];
    for (const entry of this.entries.values()) {
      if (!entry.guarded || entry.base === undefined) continue;
      out.push({
        op: "assert",
        table: entry.table,
        id: entry.id,
        expect: entry.base === null ? { exists: false } : { version: entry.base._version as number },
      });
    }
    return out;
  }

  /** The mutation's operations: the derived asserts, then the buffered writes in order. */
  mutation(): { ops: Op[]; asserts: number } {
    const asserts = this.preconditions();
    return { ops: [...asserts, ...this.ops], asserts: asserts.length };
  }
}
