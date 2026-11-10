/**
 * IR-3 properties of the recording executor and the handler retry (plan §6.6 "precondition derivation
 * properties"), over random handlers against the in-memory `/v1/ops` model (`tests/unit/sdk_model.ts`):
 *
 * 1. **Derived expects are exactly the reads.** The mutation's asserts are one per document the handler
 *    `get` from the server (not one it inserted), with the version it saw (or `exists: false`), all
 *    before the first write; the writes follow in the handler's order. A `find` adds none (v1). A run
 *    that wrote nothing sends no commit at all: its reads were one snapshot already.
 * 2. **Read-your-writes.** Every read inside the handler answers what the same read answers on the
 *    database with the handler's writes so far applied; after the commit, the database is that state.
 * 3. **Retries converge.** Concurrent read-modify-write handlers under injected faults (shed, aborted,
 *    expired, lost replies, refused connections) and conflicts lose no update and apply none twice:
 *    `successes ≤ final ≤ successes + ambiguous failures`, and with no faults `final = calls`.
 */
import assert from "node:assert/strict";
import fc from "fast-check";
import { Effect, Either } from "effect";
import type { SelectOp } from "../../core/ir/types.ts";
import type { Filter } from "../../core/plan.ts";
import { RuleError, Unavailable } from "../../core/errors.ts";
import { createClient } from "../../sdk/client.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import { defineSchema, defineTable, v } from "../../sdk/schema.ts";
import { type Doc, evalSelect, type Fault, Model, modelFetch } from "../unit/sdk_model.ts";
import { titled } from "./arbitraries.ts";

const schema = defineSchema({
  items: defineTable({ tag: v.enum("a", "b", "c"), rank: v.integer(), note: v.nullable(v.string()) }),
  counters: defineTable({ n: v.integer() }),
});
const { mutation } = defineFunctions(schema);
type Body = { tag: "a" | "b" | "c"; rank: number; note: string | null };

const SEEDED = ["i1", "i2", "i3", "i4", "i5"];
const tagArb = fc.constantFrom("a" as const, "b" as const, "c" as const);
const bodyArb: fc.Arbitrary<Body> = fc.record({
  tag: tagArb,
  rank: fc.integer({ min: 0, max: 5 }),
  note: fc.option(fc.constantFrom("x", "y", "z"), { nil: null }),
});
const seedArb = fc.subarray(SEEDED).chain((ids) =>
  fc.tuple(...ids.map(() => fc.tuple(bodyArb, fc.integer({ min: 1, max: 9 })))).map((rows) =>
    rows.map(([body, version], i): Doc => ({
      ...body,
      _id: ids[i],
      _creationTime: 100 + i,
      _version: version,
    }))
  )
);
const selectArb: fc.Arbitrary<SelectOp> = fc.record({
  filters: fc.oneof(
    fc.constant<Filter[]>([]),
    tagArb.map((value) => [{ field: "tag", op: "eq" as const, value }]),
    fc.integer({ min: 0, max: 5 }).map((value) => [{ field: "rank", op: "gt" as const, value }]),
    fc.option(fc.constantFrom("x", "y"), { nil: null }).map((
      value,
    ) => [{ field: "note", op: "eq" as const, value }]),
  ),
  order: fc.option(
    fc.record({
      field: fc.constantFrom("rank", "note", "_creationTime"),
      direction: fc.constantFrom("asc" as const, "desc" as const),
    }),
    { nil: undefined },
  ),
  limit: fc.integer({ min: 1, max: 4 }),
  aggregate: fc.option(
    fc.oneof(
      fc.constant({ kind: "count" as const }),
      fc.constantFrom("tag", "note").map((field) => ({ kind: "countBy" as const, field })),
    ),
    { nil: undefined },
  ),
}).map((r) => ({ op: "select" as const, table: "items", ...r }));

type Action =
  | { k: "get"; id: string }
  | { k: "find"; select: SelectOp }
  | { k: "insert"; body: Body }
  | { k: "patch"; pick: number; body: Partial<Body> }
  | { k: "delete"; pick: number };
const actionArb: fc.Arbitrary<Action> = fc.oneof(
  fc.constantFrom(...SEEDED, "zz").map((id) => ({ k: "get" as const, id })),
  selectArb.map((select) => ({ k: "find" as const, select })),
  bodyArb.map((body) => ({ k: "insert" as const, body })),
  fc.tuple(
    fc.nat(),
    bodyArb.chain((b) =>
      fc.subarray(Object.keys(b)).map((keys) =>
        Object.fromEntries(keys.map((k) => [k, b[k as keyof Body]])) as Partial<Body>
      )
    ),
  ).map(([pick, body]) => ({ k: "patch" as const, pick, body })),
  fc.nat().map((pick) => ({ k: "delete" as const, pick })),
);

/** Runs `select` as the SDK's `Find` would build it. */
function find(db: Parameters<Parameters<typeof mutation>[0]["handler"]>[0]["db"], op: SelectOp) {
  let f = db.find("items");
  for (const filter of op.filters) {
    f = filter.op === "eq"
      ? f.where(filter.field as "tag", filter.value as "a")
      : f.gt(filter.field as "rank", filter.value as number);
  }
  if (op.order) f = f.sort(op.order.field as "rank", op.order.direction);
  f = f.limit(op.limit);
  if (op.aggregate?.kind === "count") return f.count();
  if (op.aggregate?.kind === "countBy") return f.countBy(op.aggregate.field as "tag");
  return f.collect();
}

Deno.test(
  titled("sdk property: derived asserts are exactly the gets, writes follow, reads see the run's writes"),
  async () => {
    await fc.assert(
      fc.asyncProperty(seedArb, fc.array(actionArb, { maxLength: 12 }), async (seed, actions) => {
        const model = new Model();
        model.seed("items", seed);
        const shadow = model.clone(); // the server state with this run's writes applied, step by step
        const alive = new Set(seed.map((d) => d._id));
        const got = new Map<string, Doc | null>(); // first server state of every id the handler `get`s
        const inserted = new Set<string>();
        let n = 0, wrote = false;
        const handler = mutation({
          args: {},
          handler: ({ db }) =>
            Effect.gen(function* () {
              for (const a of actions) {
                const live = [...alive].sort();
                switch (a.k) {
                  case "get": {
                    if (!inserted.has(a.id) && !got.has(a.id)) {
                      got.set(a.id, model.table("items").get(a.id) ?? null);
                    }
                    assert.deepEqual(
                      yield* db.get("items", a.id),
                      shadow.table("items").get(a.id) ?? null,
                      `get ${a.id}`,
                    );
                    break;
                  }
                  case "find":
                    assert.deepEqual(
                      yield* find(db, a.select),
                      evalSelect(shadow.table("items").values(), a.select),
                    );
                    break;
                  case "insert": {
                    wrote = true;
                    const id = yield* db.insert("items", a.body);
                    inserted.add(id);
                    alive.add(id);
                    shadow.table("items").set(id, { ...a.body, _id: id, _creationTime: 1000, _version: 1 });
                    break;
                  }
                  case "patch": {
                    if (live.length === 0) {
                      break;
                    }
                    const id = live[a.pick % live.length];
                    wrote = true;
                    yield* db.patch("items", id, a.body);
                    const doc = shadow.table("items").get(id)!;
                    shadow.table("items").set(id, { ...doc, ...a.body, _version: doc._version + 1 });
                    break;
                  }
                  case "delete": {
                    if (live.length === 0) break;
                    const id = live[a.pick % live.length];
                    wrote = true;
                    yield* db.delete("items", id);
                    alive.delete(id);
                    shadow.table("items").delete(id);
                  }
                }
              }
              return null;
            }),
        });
        const client = createClient({
          functions: { handler },
          schema,
          tenant: "t",
          app: "a",
          endpoint: "http://agent.test",
          token: "t",
          fetch: modelFetch(model),
          newId: () => `n${String(++n).padStart(2, "0")}`, // sorts after the seeded ids, like a fresh UUIDv7
          now: () => 1000,
        });
        await Effect.runPromise(client.mutation("handler", {}));
        const commit = model.log.filter((r) => r.kind === "mutation");
        const asserts = [...got].map(([id, doc]) => ({
          op: "assert",
          table: "items",
          id,
          expect: doc === null ? { exists: false } : { version: doc._version },
        }));
        assert.equal(commit.length, wrote ? 1 : 0, "a commit exactly when the run wrote");
        if (!wrote) return;
        const ops = commit[0].ops;
        const head = ops.filter((op) => op.op === "assert");
        assert.deepEqual(ops.slice(0, head.length), head, "asserts come first");
        const byId = (a: { id: unknown }, b: { id: unknown }) => String(a.id).localeCompare(String(b.id));
        assert.deepEqual(
          [...head].sort(byId),
          asserts.sort(byId),
          "one assert per document read, at its version",
        );
        assert.ok(
          ops.slice(head.length).every((op) => op.op !== "assert" && op.op !== "get" && op.op !== "select"),
        );
        assert.deepEqual(
          [...model.table("items").values()].sort(byId as never),
          [...shadow.table("items").values()].sort(byId as never),
          "the committed state is what the handler saw",
        );
      }),
    );
  },
);

Deno.test(titled("sdk property: concurrent read-modify-write retries converge under faults"), async () => {
  const faultArb: fc.Arbitrary<Fault | undefined> = fc.oneof(
    { weight: 6, arbitrary: fc.constant(undefined) },
    fc.constant<Fault>({ kind: "shed" }),
    fc.constant<Fault>({ kind: "aborted" }),
    fc.constant<Fault>({ kind: "expired" }),
    fc.constant<Fault>({ kind: "lost" }),
    fc.constant<Fault>({ kind: "refused" }),
  );
  await fc.assert(
    fc.asyncProperty(
      fc.integer({ min: 1, max: 4 }),
      fc.integer({ min: 1, max: 4 }),
      fc.array(faultArb, { minLength: 64, maxLength: 64 }),
      fc.boolean(),
      async (clients, calls, faults, faulty) => {
        const model = new Model();
        model.seed("counters", [{ _id: "c", _creationTime: 1, _version: 1, n: 0 }]);
        const increment = mutation({
          args: {},
          handler: ({ db }) =>
            Effect.gen(function* () {
              const doc = yield* db.get("counters", "c");
              yield* db.patch("counters", "c", { n: doc!.n + 1 });
            }),
        });
        let ids = 0;
        const fetchFn = modelFetch(model, (_r, i) => faulty ? faults[i % faults.length] : undefined);
        const outcomes = await Promise.all(Array.from({ length: clients }, (_, c) => {
          const client = createClient({
            functions: { increment },
            schema,
            tenant: "t",
            app: "a",
            endpoint: "http://agent.test",
            token: "t",
            fetch: fetchFn,
            handlerRetries: 64,
            newRequestId: () => `c${c}-${++ids}`,
            sleep: () => Effect.void,
          });
          return Promise.all(
            Array.from(
              { length: calls },
              () => Effect.runPromise(Effect.either(client.mutation("increment", {}))),
            ),
          );
        }));
        const flat = outcomes.flat();
        const ok = flat.filter(Either.isRight).length;
        const ambiguous = flat.filter((o) =>
          Either.isLeft(o) && o.left instanceof Unavailable && o.left.mayHaveApplied
        )
          .length;
        const n = model.table("counters").get("c")!.n as number;
        assert.ok(ok <= n && n <= ok + ambiguous, `ok=${ok} ambiguous=${ambiguous} n=${n}`);
        if (!faulty) assert.equal(n, clients * calls);
        // Every applied commit was applied once: its request id is recorded once and the counter moved once.
        assert.equal(model.table("counters").get("c")!._version, 1 + n);
      },
    ),
    { numRuns: 100 },
  );
});

Deno.test(
  titled("sdk property: find near MAX_LIMIT after writes is exact or refused, never silently short"),
  async () => {
    await fc.assert(
      fc.asyncProperty(
        fc.integer({ min: 990, max: 1010 }),
        fc.array(
          fc.tuple(
            fc.constantFrom("delete" as const, "patch" as const, "insert" as const),
            fc.nat(),
            fc.nat(50),
          ),
          { maxLength: 8 },
        ),
        fc.integer({ min: 990, max: 1000 }),
        fc.option(fc.constantFrom("asc" as const, "desc" as const), { nil: undefined }),
        fc.constantFrom("rows" as const, "count" as const, "countBy" as const),
        async (size, writes, limit, direction, shape) => {
          const model = new Model();
          const ids = Array.from({ length: size }, (_, i) => `d${String(i).padStart(5, "0")}`);
          model.seed(
            "items",
            ids.map((id, i) => ({ _id: id, _creationTime: 1, _version: 1, tag: "a", rank: i, note: null })),
          );
          const shadow = model.clone();
          const select: SelectOp = {
            op: "select",
            table: "items",
            filters: [],
            limit,
            ...(direction ? { order: { field: "rank", direction } } : {}),
            ...(shape === "count"
              ? { aggregate: { kind: "count" as const } }
              : shape === "countBy"
              ? { aggregate: { kind: "countBy" as const, field: "rank" } }
              : {}),
          };
          const displaced = new Set<string>();
          let answer: unknown;
          let n = 0;
          const handler = mutation({
            args: {},
            handler: ({ db }) =>
              Effect.gen(function* () {
                for (const [kind, pick, rank] of writes) {
                  const live = [...shadow.table("items").keys()].sort();
                  if (kind === "insert") {
                    const id = yield* db.insert("items", { tag: "b", rank, note: null });
                    shadow.table("items").set(id, {
                      _id: id,
                      _creationTime: 1000,
                      _version: 1,
                      tag: "b",
                      rank,
                      note: null,
                    });
                    continue;
                  }
                  const id = live[pick % live.length];
                  if (id.startsWith("d")) displaced.add(id);
                  if (kind === "delete") {
                    yield* db.delete("items", id);
                    shadow.table("items").delete(id);
                  } else {
                    yield* db.patch("items", id, { rank });
                    const doc = shadow.table("items").get(id)!;
                    shadow.table("items").set(id, { ...doc, rank, _version: doc._version + 1 });
                  }
                }
                answer = yield* Effect.either(find(db, select) as Effect.Effect<unknown, unknown>);
              }),
          });
          const client = createClient({
            functions: { handler },
            schema,
            tenant: "t",
            app: "a",
            endpoint: "http://agent.test",
            token: "t",
            fetch: modelFetch(model),
            newId: () => `n${String(++n).padStart(2, "0")}`,
            now: () => 1000,
          });
          await Effect.runPromise(client.mutation("handler", {}));
          const result = answer as Either.Either<unknown, unknown>;
          if (Either.isRight(result)) {
            assert.deepEqual(result.right, evalSelect(shadow.table("items").values(), select));
          } else {
            assert.ok(
              result.left instanceof RuleError && result.left.rule === "find-limit",
              String(result.left),
            );
            assert.ok(limit + displaced.size > 1000, "refused only when the widened limit was capped");
          }
        },
      ),
      { numRuns: 60 },
    );
  },
);
