/**
 * The recording executor (IR-3, `docs/plans/ir-protocol.md` §3): runs a handler in the caller's process
 * against the remote database, and turns what it did into one IR request.
 *
 * - **Reads go out at once** as query requests (`Reads`) and are cached per handler run: a second `get` of
 *   the same document answers from the cache. Reads asked for together are one request:
 *   `db.getMany(table, ids)`, or any reads under `Effect.all([...], { batching: true })`. Without
 *   `batching: true` each read is its own request, concurrent or not. A read that depends on another's
 *   answer is its own round trip; nothing outside the server can batch that.
 * - **One run, one snapshot.** Every answer carries the database's commit sequence (spec §6.5), and every
 *   read request after the first carries, in front of its reads, the asserts of what the run has read so
 *   far: the server checks them in the same snapshot it reads from. So after each answer, everything the
 *   run has read is known to hold in one snapshot, `snapshot`. If an assert fails, the run is abandoned
 *   (`Restart`) and the client runs the handler again. Reads too many to share a request with the asserts
 *   are sent bare and must all answer the same sequence.
 * - **Writes are buffered.** `insert` draws a client id (UUIDv7) and answers it at once; `patch` and
 *   `delete` answer nothing. Later reads in the same run see them (read-your-writes): a `get` answers the
 *   local state, and a `find` corrects the server's answer with `sdk/overlay.ts` (one query request:
 *   the widened select plus a `get` of each document the run patched or deleted).
 * - **Preconditions are derived from the reads.** Every document a handler `get` observed on the server
 *   becomes an `assert` at the front of the mutation: `{version: v}` if it existed, `{exists: false}` if
 *   it did not. Every `find` becomes a table assert, `{unchangedSince: s}` with `s` the sequence its
 *   answer was read at: no mutation has changed a row of that table since, so whatever the select
 *   answered it still answers, and a decision that depends on it ("no row matches, so insert one") is
 *   protected against phantoms. The guard is coarse on purpose (any change to the table fails it, a false
 *   conflict that costs a re-run); `find(...).unguarded()` opts a display-only read out. The documents a
 *   `find` returns are not asserted one by one: the table assert covers them.
 *
 * - **A conflict is also an answer.** A failed assert comes back with the commit sequence the server
 *   checked at and the current state behind every assert that failed (spec §8.1). `rebase` turns that
 *   and this run's read set into a `ReadSet`: everything the run read, as it is at that sequence. The next
 *   run starts from it (`seed`) and reads only what it has not got, so a re-run of a handler that reads by
 *   id costs one round trip (its commit), not its reads again. What the next run uses of the seed it
 *   asserts again, like any read.
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
import { Effect, Request, RequestResolver } from "effect";
import { type DbError, NotFoundError, RemoteError, RuleError } from "../core/errors.ts";
import {
  type AssertOp,
  type GetOp,
  MAX_OPS,
  type Op,
  type SelectOp,
  type TableAssertOp,
} from "../core/ir/types.ts";
import { type Equality, type Execute, Find, type Operation, type SelectPlan, type WriteDB } from "./db.ts";
import type { Body, Document, Schema } from "./schema.ts";
import type { IdSource } from "./ids.ts";
import { type Doc, overlay, widenedLimit } from "./overlay.ts";
import { MAX_LIMIT } from "../core/plan.ts";

export type ReadOp = GetOp | SelectOp;

/** A query request's answer: one result per op, and the commit sequence of the snapshot they were read in. */
export interface Answer {
  readonly result: unknown[];
  readonly seq: number;
}

/**
 * Sends one query request (1–64 `assert`/`get`/`select` ops). It fails with the server's `ConflictError`
 * when an assert does not hold. Any other failure is not a `DbError`: the client raises it as a defect it
 * recognises (`sdk/client.ts`), so it reaches the caller as a `ClientError` without widening the handler's
 * error channel.
 *
 * This is the executor's only way out: it knows no transport, so the same executor can run against
 * anything that answers reads at a named sequence (a server, a test model, later a local store).
 */
export type Reads = (ops: (Guard | ReadOp)[]) => Effect.Effect<Answer, RemoteError>;

/**
 * A derived precondition: a document at the version a `get` saw, or a table unchanged since the sequence
 * a `find` was answered at. They are plain IR asserts, so anything that runs a handler (this client, later
 * a local speculative run) states its read set the same way.
 */
export type Guard = AssertOp | TableAssertOp;

/**
 * Reads that hold together at commit sequence `seq`: what a run may start from instead of asking again.
 * It is plain data, with nothing of the transport in it, so whatever can produce one (a conflict answer
 * today, later a local store) can seed a run.
 */
export interface ReadSet {
  readonly seq: number;
  /** Documents by id; `null` where there is none. */
  readonly docs: readonly { readonly table: string; readonly id: string; readonly doc: Doc | null }[];
  /** Select answers, each from a table unchanged since `since`. */
  readonly finds: readonly Found[];
}
interface Found {
  readonly op: SelectOp;
  readonly value: unknown;
  readonly since: number;
}

/**
 * The defect that abandons a run whose reads no longer hold in one snapshot. `error` is the conflict to
 * report if the client stops re-running; `seed` is what the next run may start from, when the conflict
 * said enough.
 */
export class Restart {
  constructor(readonly error: RemoteError, readonly seed?: ReadSet) {}
}

/** What a `Recording` is given. */
export interface RecordingOptions {
  readonly reads: Reads;
  /** Document ids for inserts. */
  readonly ids: IdSource;
  readonly now: () => number;
  /** False for a query handler: it gets the read half only (the types already forbid writes). */
  readonly writable: boolean;
  /** Reads to start from (the last run's, rebased on its conflict). */
  readonly seed?: ReadSet;
}

/** One read waiting for its batch; it answers its result and the sequence it was read at. */
interface Fetch extends Request.Request<readonly [result: unknown, seq: number]> {
  readonly _tag: "Fetch";
  readonly op: ReadOp;
}
const Fetch = Request.tagged<Fetch>("Fetch");

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
  /**
   * The commit sequence of the snapshot in which everything this run has read from the server holds;
   * `undefined` until the first answer.
   */
  snapshot: number | undefined;
  /** The tables this run's guarded selects read, each with the sequence of its first answer. */
  private readonly tables = new Map<string, number>();
  /** The guarded selects answered without local writes in the way, by their op's JSON. */
  private readonly found = new Map<string, Found>();
  private readonly reads: Reads;
  private readonly ids: IdSource;
  private readonly now: () => number;
  private readonly writable: boolean;
  /** Reads requested together are one `fetch`. */
  private readonly batch = RequestResolver.makeBatched((requests: readonly Fetch[]) =>
    this.fetch(requests.map((request) => request.op)).pipe(
      Effect.flatMap(({ results, seq }) =>
        Effect.forEach(requests, (request, i) => Request.succeed(request, [results[i], seq] as const), {
          discard: true,
        })
      ),
      Effect.catchAllCause((cause) =>
        Effect.forEach(requests, (request) => Request.failCause(request, cause), { discard: true })
      ),
    )
  );

  constructor(options: RecordingOptions) {
    this.reads = options.reads;
    this.ids = options.ids;
    this.now = options.now;
    this.writable = options.writable;
    if (options.seed) {
      this.snapshot = options.seed.seq;
      for (const { table, id, doc } of options.seed.docs) this.entry(table, id).base = doc;
      for (const find of options.seed.finds) this.found.set(JSON.stringify(find.op), find);
    }
  }

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

  /** One read, batched with the others requested at the same time. */
  private read(op: ReadOp): Effect.Effect<readonly [result: unknown, seq: number]> {
    return Effect.request(Fetch({ op }), this.batch).pipe(Effect.withRequestCaching(false));
  }

  /**
   * Sends `ops` and answers their results and the sequence they were read at, keeping the run on one
   * snapshot (see the module comment). Dies with `Restart` when the run's earlier reads no longer hold.
   */
  private fetch(ops: ReadOp[]): Effect.Effect<{ results: unknown[]; seq: number }> {
    return Effect.gen(this, function* () {
      const asserts = this.preconditions();
      const restart = (error: RemoteError) => Effect.die(new Restart(error));
      if (asserts.length + ops.length <= MAX_OPS) {
        const answer = yield* Effect.catchAll(
          this.reads([...asserts, ...ops]),
          (error) => Effect.die(new Restart(error, this.rebase(error, asserts))),
        );
        this.snapshot = answer.seq;
        return { results: answer.result.slice(asserts.length), seq: answer.seq };
      }
      const results: unknown[] = [];
      for (let i = 0; i < ops.length; i += MAX_OPS) {
        const answer = yield* Effect.catchAll(this.reads(ops.slice(i, i + MAX_OPS)), restart);
        this.snapshot ??= answer.seq;
        if (answer.seq !== this.snapshot) {
          return yield* restart(
            new RemoteError({
              status: 409,
              remoteTag: "ConflictError",
              message: `the database changed between the reads of one run (sequence ${this.snapshot} to ` +
                `${answer.seq}), and they are too many to check in one request`,
            }),
          );
        }
        results.push(...answer.result);
      }
      return { results, seq: this.snapshot! };
    });
  }

  get<K extends keyof S & string>(table: K, id: string): Effect.Effect<Document<S, K> | null, DbError> {
    const entry = this.entry(table, id);
    return Effect.gen(this, function* () {
      if (!this.known(entry)) {
        const [fetched] = (yield* this.read({ op: "get", table, id })) as [Doc | null, number];
        entry.base ??= fetched; // a read made at the same time may have answered first
      }
      // Guard what the server told this run, not what the run wrote itself.
      if (entry.base !== undefined && entry.writes[0]?.op !== "insert") entry.guarded = true;
      return (yield* this.view(entry)) as Document<S, K> | null;
    });
  }

  getMany<K extends keyof S & string>(
    table: K,
    ids: readonly string[],
  ): Effect.Effect<(Document<S, K> | null)[], DbError> {
    return Effect.all(ids.map((id) => this.get(table, id)), { batching: true });
  }

  find<K extends keyof S & string>(table: K, where: Equality<Document<S, K>> = {}): Find<Document<S, K>> {
    const execute: Execute = (operation: Operation, guarded = true) =>
      operation.kind === "select"
        ? this.select(operation, guarded)
        : Effect.dieMessage("find executes selects only");
    return new Find(execute, {
      kind: "select",
      table,
      limit: 100,
      filters: Object.entries(where).map(([field, value]) => ({ field, op: "eq", value })),
    });
  }

  /**
   * A select, corrected for this run's writes to its table (read-your-writes). Guarded: the table joins
   * the read set at the sequence the server answered at.
   */
  private select(plan: SelectPlan, guarded: boolean): Effect.Effect<unknown, DbError> {
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
      const guard = (seq: number) => {
        if (guarded && !this.tables.has(plan.table)) this.tables.set(plan.table, seq);
      };
      if (touched.length === 0) {
        const known = guarded ? this.found.get(JSON.stringify(op)) : undefined;
        if (known) {
          guard(known.since);
          return structuredClone(known.value); // the handler may keep and change what it is given
        }
        const [value, seq] = yield* this.read(op);
        guard(seq);
        if (guarded) this.found.set(JSON.stringify(op), { op, value, since: seq });
        return value;
      }
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
      const { results: [server, ...current], seq } = yield* this.fetch([
        { ...op, limit: requested },
        ...displaced.map(({ table, id }): GetOp => ({ op: "get", table, id })),
      ]);
      guard(seq);
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

  /** The preconditions derived from this run's reads: the documents in first-read order, then the tables. */
  preconditions(): Guard[] {
    const out: Guard[] = [];
    for (const entry of this.entries.values()) {
      if (!entry.guarded || entry.base === undefined) continue;
      out.push({
        op: "assert",
        table: entry.table,
        id: entry.id,
        expect: entry.base === null ? { exists: false } : { version: entry.base._version as number },
      });
    }
    for (const [table, unchangedSince] of this.tables) {
      out.push({ op: "assert", table, expect: { unchangedSince } });
    }
    return out;
  }

  /**
   * This run's read set as it is at the sequence a conflict was decided at, for the next run to start
   * from. `error` is the `ConflictError` of a request whose first operations were `sent`, guards this
   * recording derived. `undefined` when the conflict does not say enough (no `seq` or `current`, or a
   * `current` cut short): the next run then reads everything again.
   */
  rebase(error: RemoteError, sent: readonly Guard[]): ReadSet | undefined {
    const { seq, current, currentTruncated } = error.fields ?? {};
    if (typeof seq !== "number" || !Array.isArray(current) || currentTruncated) return undefined;
    // The server lists every assert that fails from the first failing one on; the ones before it held.
    const failed = new Map((current as { op: number; doc?: Doc | null }[]).map((state) => [state.op, state]));
    const docs: { table: string; id: string; doc: Doc | null }[] = [];
    const unchanged = new Set<string>();
    sent.forEach((guard, i) => {
      const now = failed.get(i);
      if (!("id" in guard)) {
        if (!now) unchanged.add(guard.table);
        return;
      }
      const id = guard.id as string;
      const doc = now ? now.doc ?? null : this.entry(guard.table, id).base;
      if (doc !== undefined) docs.push({ table: guard.table, id, doc });
    });
    return { seq, docs, finds: [...this.found.values()].filter((find) => unchanged.has(find.op.table)) };
  }

  /** The mutation's operations: the derived asserts, then the buffered writes in order. */
  mutation(): { ops: Op[]; asserts: number } {
    const asserts = this.preconditions();
    return { ops: [...asserts, ...this.ops], asserts: asserts.length };
  }
}
