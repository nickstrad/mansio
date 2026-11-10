/**
 * The SDK's read path (survey changes 1–4, `docs/programmatic-schema-platform-survey.md` §3.2) against the
 * in-memory `/v1/ops` model (`sdk_model.ts`), through the real HTTP transport (a fake `fetch`). Every test
 * counts round trips: `model.log` is every request the server received.
 */
import assert from "node:assert/strict";
import { Effect } from "effect";
import type { Op, Request } from "../../core/ir/types.ts";
import { functions as app } from "../../app/functions.ts";
import { schema as appSchema } from "../../app/schema.ts";
import { RuleError } from "../../core/errors.ts";
import { memoryCache } from "../../sdk/cache.ts";
import { type ClientOptions, createClient } from "../../sdk/client.ts";
import { defineFunctions } from "../../sdk/functions.ts";
import { defineSchema, defineTable, v } from "../../sdk/schema.ts";
import { type Doc, type Fault, Model, modelFetch } from "./sdk_model.ts";

const schema = defineSchema({
  accounts: defineTable({ owner: v.string(), balance: v.integer() }),
  names: defineTable({ name: v.string() }),
});
const { query, mutation } = defineFunctions(schema);

const functions = {
  /** Two reads asked for together. */
  pair: query({
    args: { a: v.string(), b: v.string() },
    handler: ({ db }, { a, b }) =>
      Effect.all([db.get("accounts", a), db.get("accounts", b)], { batching: true }),
  }),
  /** The same, with the reads also run concurrently. */
  pairConcurrently: query({
    args: { a: v.string(), b: v.string() },
    handler: ({ db }, { a, b }) =>
      Effect.all([db.get("accounts", a), db.get("accounts", b)], {
        concurrency: "unbounded",
        batching: true,
      }),
  }),
  many: query({
    args: { ids: v.array(v.string()) },
    handler: ({ db }, { ids }) => db.getMany("accounts", ids),
  }),
  /** A get and a find asked for together. */
  mixed: query({
    args: { a: v.string() },
    handler: ({ db }, { a }) =>
      Effect.all([db.get("accounts", a), db.find("accounts", { owner: "ada" }).count()], { batching: true }),
  }),
  /** Two reads, the second after the first: two round trips, one snapshot. */
  total: query({
    args: { a: v.string(), b: v.string() },
    handler: ({ db }, { a, b }) =>
      Effect.gen(function* () {
        const x = yield* db.get("accounts", a);
        const y = yield* db.get("accounts", b);
        return (x?.balance ?? 0) + (y?.balance ?? 0);
      }),
  }),
  /** A mutation that only reads. */
  peek: mutation({
    args: { a: v.string() },
    handler: ({ db }, { a }) => db.get("accounts", a),
  }),
  /** "No row has this name, so insert one": a decision on what a find did not see. */
  claim: mutation({
    args: { name: v.string() },
    handler: ({ db }, { name }) =>
      Effect.gen(function* () {
        const taken = yield* db.find("names", { name }).first();
        return taken
          ? { id: taken._id, created: false }
          : { id: yield* db.insert("names", { name }), created: true };
      }),
  }),
  /** The same decision with the guard switched off. */
  claimUnguarded: mutation({
    args: { name: v.string() },
    handler: ({ db }, { name }) =>
      Effect.gen(function* () {
        const taken = yield* db.find("names", { name }).unguarded().first();
        return taken
          ? { id: taken._id, created: false }
          : { id: yield* db.insert("names", { name }), created: true };
      }),
  }),
  /** Two finds, one after the other. */
  census: query({
    args: {},
    handler: ({ db }) =>
      Effect.gen(function* () {
        const names = yield* db.find("names").count();
        const accounts = yield* db.find("accounts").count();
        return { names, accounts };
      }),
  }),
  transfer: mutation({
    args: { from: v.string(), to: v.string(), amount: v.integer() },
    handler: ({ db }, { from, to, amount }) =>
      Effect.gen(function* () {
        const [x, y] = yield* db.getMany("accounts", [from, to]);
        if (!x || !y) return false;
        yield* db.patch("accounts", from, { balance: x.balance - amount });
        yield* db.patch("accounts", to, { balance: y.balance + amount });
        return true;
      }),
  }),
};

const account = (id: string, balance: number, version = 1): Doc => ({
  _id: id,
  _creationTime: 1,
  _version: version,
  owner: "ada",
  balance,
});

function setup(
  fault?: (request: Request, n: number) => Fault | undefined,
  over: Partial<ClientOptions<typeof functions>> = {},
) {
  const model = new Model();
  let ids = 0, requests = 0;
  const sleeps: number[] = [];
  const client = createClient({
    functions,
    schema,
    tenant: "acme",
    app: "bank",
    endpoint: "http://agent.test",
    token: "t0",
    fetch: modelFetch(model, fault),
    newId: () => `id-${++ids}`,
    newRequestId: () => `req-${++requests}`,
    now: () => 1000,
    random: () => 0.5,
    sleep: (ms) => Effect.sync(() => void sleeps.push(ms)),
    ...over,
  });
  return { model, client, sleeps };
}

const run = Effect.runPromise;
const ops = (model: Model): Op[][] => model.log.map((request) => request.ops);
const kinds = (model: Model) => model.log.map((request) => request.kind);
/** Moves `amount` from a1 to a2 behind the client's back, as another writer would. */
function moveBehind(model: Model, amount: number) {
  for (const [id, by] of [["a1", -amount], ["a2", amount]] as const) {
    const doc = model.table("accounts").get(id)!;
    model.exec({
      v: 1,
      db: { tenant: "acme", app: "bank" },
      kind: "mutation",
      requestId: `behind-${id}-${doc._version}`,
      schema: "",
      ops: [{ op: "patch", table: "accounts", id, value: { balance: (doc.balance as number) + by } }],
    });
  }
  model.log.length -= 2; // not the client's requests
}

// ---------------------------------------------------------------------------
// Change 1: batched reads, one snapshot per run
// ---------------------------------------------------------------------------

Deno.test("read path 1: reads asked for together are one round trip", async () => {
  const { model, client } = setup();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  const get = (id: string): Op => ({ op: "get", table: "accounts", id });

  assert.deepEqual(await run(client.query("pair", { a: "a1", b: "a2" })), [
    account("a1", 10),
    account("a2", 5),
  ]);
  assert.deepEqual(ops(model), [[get("a1"), get("a2")]]);

  model.log.length = 0;
  await run(client.query("pairConcurrently", { a: "a1", b: "a2" }));
  assert.deepEqual(ops(model), [[get("a1"), get("a2")]]);

  model.log.length = 0;
  assert.deepEqual(await run(client.query("many", { ids: ["a2", "nope", "a1", "a2"] })), [
    account("a2", 5),
    null,
    account("a1", 10),
    account("a2", 5),
  ]);
  assert.equal(model.log.length, 1);

  model.log.length = 0;
  assert.deepEqual(await run(client.query("mixed", { a: "a1" })), [account("a1", 10), 2]);
  assert.equal(model.log.length, 1);
  assert.deepEqual(model.log[0].ops.map((op) => op.op), ["get", "select"]);
});

Deno.test("read path 1: a later read carries the asserts of the earlier ones, so a run is one snapshot", async () => {
  const { model, client } = setup();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  assert.equal(await run(client.query("total", { a: "a1", b: "a2" })), 15);
  assert.deepEqual(ops(model), [
    [{ op: "get", table: "accounts", id: "a1" }],
    [
      { op: "assert", table: "accounts", id: "a1", expect: { version: 1 } },
      { op: "get", table: "accounts", id: "a2" },
    ],
  ]);
});

Deno.test("read path 1: a write between two reads of one run never shows a mixed snapshot", async () => {
  // The invariant is a1 + a2 = 15. A transfer lands between the handler's two reads: without one snapshot
  // the handler would add the old a1 to the new a2 and answer 18.
  const { model, client, sleeps } = setup((_request, n) =>
    n === 1 ? { kind: "before", run: () => moveBehind(model, 3) } : undefined
  );
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  assert.equal(await run(client.query("total", { a: "a1", b: "a2" })), 15);
  // Run 1: the read of a1, then the read of a2 whose assert on a1 fails. Run 2 has a1 from the conflict
  // (change 3) and reads a2, asserting a1 as the conflict gave it.
  assert.deepEqual(kinds(model), ["query", "query", "query"]);
  assert.equal(sleeps.length, 1);
  assert.deepEqual(model.log[2].ops, [
    { op: "assert", table: "accounts", id: "a1", expect: { version: 2 } },
    { op: "get", table: "accounts", id: "a2" },
  ]);
});

Deno.test("read path 1: a run whose snapshot keeps moving stops after handlerRetries with the conflict", async () => {
  const { model, client } = setup(
    (request) =>
      request.ops[0].op === "assert" ? { kind: "before", run: () => moveBehind(model, 1) } : undefined,
    { handlerRetries: 2 },
  );
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  const out = await run(Effect.either(client.query("total", { a: "a1", b: "a2" })));
  assert.equal(out._tag, "Left");
  if (out._tag === "Left") {
    assert.equal(out.left._tag, "RemoteError");
    assert.equal((out.left as { remoteTag: string }).remoteTag, "ConflictError");
  }
  assert.equal(model.log.length, 4); // the read of a1, then three runs' reads of a2, each refused
});

Deno.test("read path 1: a mutation that wrote nothing sends no commit; one that writes asserts what it read", async () => {
  const { model, client } = setup();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  assert.deepEqual(await run(client.mutation("peek", { a: "a1" })), account("a1", 10));
  assert.deepEqual(kinds(model), ["query"]);

  model.log.length = 0;
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 4 })), true);
  assert.deepEqual(kinds(model), ["query", "mutation"]); // two reads in one trip, then the commit
  assert.deepEqual(model.log[1].ops.map((op) => op.op), ["assert", "assert", "patch", "patch"]);
  assert.equal(model.table("accounts").get("a1")!.balance, 6);
  assert.equal(model.table("accounts").get("a2")!.balance, 9);
});

Deno.test("read path 1: reads too many for one request are sent in parts that must share a sequence", async () => {
  const ids = Array.from({ length: 70 }, (_, i) => `b${String(i).padStart(2, "0")}`);
  let moved = false;
  const { model, client } = setup((_request, n) =>
    n === 1 && !moved ? { kind: "before", run: () => (moved = true, moveBehind(model, 1)) } : undefined
  );
  model.seed("accounts", [account("a1", 10), account("a2", 5), ...ids.map((id) => account(id, 1))]);
  const out = await run(client.query("many", { ids }));
  assert.equal(out.length, 70);
  // Run 1: 64 reads, then 6 that answer a later sequence. Run 2: both parts at one sequence.
  assert.deepEqual(model.log.map((request) => request.ops.length), [64, 6, 64, 6]);
});

// ---------------------------------------------------------------------------
// Change 2: guarded find
// ---------------------------------------------------------------------------

/** Another writer inserts `name` behind the client's back. */
function insertBehind(model: Model, id: string, name: string) {
  model.exec({
    v: 1,
    db: { tenant: "acme", app: "bank" },
    kind: "mutation",
    requestId: `behind-${id}`,
    schema: "",
    ops: [{ op: "insert", table: "names", id, value: { name } }],
  });
  model.log.length -= 1;
}
const named = (model: Model, name: string) =>
  [...model.table("names").values()].filter((d) => d.name === name);

Deno.test("read path 2: an insert decided on an empty find conflicts with a phantom instead of committing", async () => {
  // The other writer's insert lands after this run's find and before its commit.
  const { model, client } = setup((request) =>
    request.kind === "mutation" && !named(model, "ada").length
      ? { kind: "before", run: () => insertBehind(model, "other-1", "ada") }
      : undefined
  );
  const out = await run(client.mutation("claim", { name: "ada" }));
  // Run 1: find (nothing), commit refused at the table assert. Run 2: find (the other's row), nothing to write.
  assert.deepEqual(out, { id: "other-1", created: false });
  assert.deepEqual(kinds(model), ["query", "mutation", "query"]);
  assert.deepEqual(model.log[1].ops, [
    { op: "assert", table: "names", expect: { unchangedSince: 0 } },
    { op: "insert", table: "names", id: "id-1", value: { name: "ada" } },
  ]);
  assert.equal(named(model, "ada").length, 1, "one row holds the name");
});

Deno.test("read path 2: without a concurrent writer the guarded insert commits in two round trips", async () => {
  const { model, client } = setup();
  assert.deepEqual(await run(client.mutation("claim", { name: "ada" })), { id: "id-1", created: true });
  assert.deepEqual(kinds(model), ["query", "mutation"]);
  assert.deepEqual(await run(client.mutation("claim", { name: "ada" })), { id: "id-1", created: false });
});

Deno.test("read path 2: unguarded() opts a find out, and the phantom then commits twice", async () => {
  const { model, client } = setup((request) =>
    request.kind === "mutation"
      ? { kind: "before", run: () => insertBehind(model, "other-1", "ada") }
      : undefined
  );
  assert.deepEqual(await run(client.mutation("claimUnguarded", { name: "ada" })), {
    id: "id-1",
    created: true,
  });
  assert.deepEqual(model.log[1].ops.map((op) => op.op), ["insert"]);
  assert.equal(named(model, "ada").length, 2, "the write skew the guard exists to stop");
});

Deno.test("read path 2: finds join the run's snapshot: a later read asserts the tables read so far", async () => {
  let done = false;
  const { model, client } = setup((_request, n) =>
    n === 1 && !done
      ? { kind: "before", run: () => (done = true, insertBehind(model, "other-1", "ada")) }
      : undefined
  );
  model.seed("accounts", [account("a1", 10)]);
  assert.deepEqual(await run(client.query("census", {})), { names: 1, accounts: 1 });
  // Run 1's second read asserts `names` unchanged since 0 and fails; run 2 reads both at sequence 1.
  assert.deepEqual(model.log[1].ops[0], { op: "assert", table: "names", expect: { unchangedSince: 0 } });
  assert.deepEqual(model.log[3].ops[0], { op: "assert", table: "names", expect: { unchangedSince: 1 } });
  assert.equal(model.log.length, 4);
});

// ---------------------------------------------------------------------------
// Change 3: conflict answers carry the current state
// ---------------------------------------------------------------------------

Deno.test("read path 3: a conflict re-run does not re-read: one extra round trip, not the reads again", async () => {
  let moved = false;
  const { model, client } = setup((request) =>
    request.kind === "mutation" && !moved
      ? { kind: "before", run: () => (moved = true, moveBehind(model, 3)) }
      : undefined
  );
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 4 })), true);
  // Run 1: the two reads (one trip), the commit refused. Run 2: no read at all, the commit.
  assert.deepEqual(kinds(model), ["query", "mutation", "mutation"]);
  // The refused commit's answer is what run 2 computed from: both accounts as the other writer left them.
  assert.deepEqual(model.log[2].ops, [
    { op: "assert", table: "accounts", id: "a1", expect: { version: 2 } },
    { op: "assert", table: "accounts", id: "a2", expect: { version: 2 } },
    { op: "patch", table: "accounts", id: "a1", value: { balance: 3 } },
    { op: "patch", table: "accounts", id: "a2", value: { balance: 12 } },
  ]);
  assert.equal(model.table("accounts").get("a1")!.balance, 3);
  assert.equal(model.table("accounts").get("a2")!.balance, 12);
});

Deno.test("read path 3: a re-run keeps the reads the conflict did not touch, finds included", async () => {
  // `audit` reads an account and counts the names, then writes. Only the account changes under it.
  const audit = mutation({
    args: { a: v.string() },
    handler: ({ db }, { a }) =>
      Effect.gen(function* () {
        const [doc, names] = yield* Effect.all([db.get("accounts", a), db.find("names").count()], {
          batching: true,
        });
        yield* db.patch("accounts", a, { balance: doc!.balance + names });
        return names;
      }),
  });
  const model = new Model();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  model.seed("names", [{ _id: "n1", _creationTime: 1, _version: 1, name: "ada" }]);
  let moved = false;
  const client = createClient({
    functions: { audit },
    schema,
    tenant: "acme",
    app: "bank",
    endpoint: "http://agent.test",
    token: "t0",
    fetch: modelFetch(
      model,
      (request) =>
        request.kind === "mutation" && !moved
          ? { kind: "before", run: () => (moved = true, moveBehind(model, 3)) }
          : undefined,
    ),
    sleep: () => Effect.void,
  });
  assert.equal(await run(client.mutation("audit", { a: "a1" })), 1);
  assert.deepEqual(kinds(model), ["query", "mutation", "mutation"]);
  assert.deepEqual(model.log[2].ops, [
    { op: "assert", table: "accounts", id: "a1", expect: { version: 2 } },
    { op: "assert", table: "names", expect: { unchangedSince: 0 } },
    { op: "patch", table: "accounts", id: "a1", value: { balance: 8 } },
  ]);
});

Deno.test("read path 3: a conflict that does not carry the state falls back to reading again", async () => {
  // An answer without `current` (a 1.0 server, or a payload cut at the size cap): nothing is assumed.
  let moved = false;
  const model = new Model();
  const inner = modelFetch(
    model,
    (request) =>
      request.kind === "mutation" && !moved
        ? { kind: "before", run: () => (moved = true, moveBehind(model, 3)) }
        : undefined,
  );
  const stripped = (async (url: string, init?: RequestInit) => {
    const response = await inner(url, init);
    if (response.status !== 409) return response;
    const body = await response.json();
    delete body.error.fields.current;
    return new Response(JSON.stringify(body), { status: 409 });
  }) as typeof fetch;
  const { client } = setup(undefined, { fetch: stripped });
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 4 })), true);
  assert.deepEqual(kinds(model), ["query", "mutation", "query", "mutation"]);
  assert.equal(model.table("accounts").get("a1")!.balance, 3);
});

// ---------------------------------------------------------------------------
// Change 4: version-checked cached reads
// ---------------------------------------------------------------------------

Deno.test("read path 4: a warm-cache mutation is one round trip, and its commit asserts the cached versions", async () => {
  const { model, client } = setup();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  // Cold: the reads (one trip) and the commit.
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 1 })), true);
  assert.deepEqual(kinds(model), ["query", "mutation"]);
  // Warm: the first commit left both accounts in the cache at the versions it wrote.
  model.log.length = 0;
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 1 })), true);
  assert.deepEqual(kinds(model), ["mutation"]);
  assert.deepEqual(model.log[0].ops, [
    { op: "assert", table: "accounts", id: "a1", expect: { version: 2 } },
    { op: "assert", table: "accounts", id: "a2", expect: { version: 2 } },
    { op: "patch", table: "accounts", id: "a1", value: { balance: 8 } },
    { op: "patch", table: "accounts", id: "a2", value: { balance: 7 } },
  ]);
  assert.equal(model.table("accounts").get("a1")!.balance, 8);
  // A query's answer also warms the cache: a first mutation after it is one trip too.
  const other = setup();
  other.model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  await run(other.client.query("pair", { a: "a1", b: "a2" }));
  other.model.log.length = 0;
  await run(other.client.mutation("transfer", { from: "a1", to: "a2", amount: 1 }));
  assert.deepEqual(kinds(other.model), ["mutation"]);
});

Deno.test("read path 4: the cache takes the version the commit answered, not the last plus one", async () => {
  // A server restored after a crash sets versions from a floor (spec §6.3), so a patch can answer more than
  // the read version plus one. The cache must hold what the server said, or every warm run would conflict.
  const { model, client } = setup();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  model.floor = 2 ** 32;
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 1 })), true);
  model.log.length = 0;
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 1 })), true);
  assert.deepEqual(kinds(model), ["mutation"]);
  assert.deepEqual(model.log[0].ops.slice(0, 2), [
    { op: "assert", table: "accounts", id: "a1", expect: { version: 2 ** 32 } },
    { op: "assert", table: "accounts", id: "a2", expect: { version: 2 ** 32 } },
  ]);
  assert.equal(model.table("accounts").get("a1")!._version, 2 ** 32 + 1);
});

Deno.test("read path 4: a stale-cache mutation cannot commit: it is refused, then re-run on the current state", async () => {
  const { model, client } = setup();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  await run(client.query("pair", { a: "a1", b: "a2" })); // the cache now holds a1 = 10, a2 = 5 at version 1
  moveBehind(model, 7); // another writer: a1 = 3, a2 = 12, both at version 2
  model.log.length = 0;
  assert.equal(await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 1 })), true);
  // The commit computed from the stale cache asserted version 1 and was refused; nothing of it applied.
  assert.deepEqual(kinds(model), ["mutation", "mutation"]);
  assert.deepEqual(model.log[0].ops.slice(2), [
    { op: "patch", table: "accounts", id: "a1", value: { balance: 9 } },
    { op: "patch", table: "accounts", id: "a2", value: { balance: 6 } },
  ]);
  // The re-run computed from what the refusal carried: one extra round trip, and the right numbers.
  assert.deepEqual(model.log[1].ops.slice(2), [
    { op: "patch", table: "accounts", id: "a1", value: { balance: 2 } },
    { op: "patch", table: "accounts", id: "a2", value: { balance: 13 } },
  ]);
  assert.equal(model.table("accounts").get("a1")!.balance, 2);
  assert.equal(model.table("accounts").get("a2")!.balance, 13);
  // And the cache is current again: the next transfer is one trip.
  model.log.length = 0;
  await run(client.mutation("transfer", { from: "a1", to: "a2", amount: 1 }));
  assert.deepEqual(kinds(model), ["mutation"]);
});

/** Fails in the handler when the balance does not cover the amount: a decision that writes nothing. */
const withdraw = mutation({
  args: { a: v.string(), amount: v.integer() },
  handler: ({ db }, { a, amount }) =>
    Effect.gen(function* () {
      const doc = yield* db.get("accounts", a);
      if (!doc || doc.balance < amount) {
        return yield* new RuleError({ rule: "insufficient", detail: `balance ${doc?.balance}` });
      }
      yield* db.patch("accounts", a, { balance: doc.balance - amount });
      return doc.balance - amount;
    }),
});

function bank(over: Partial<ClientOptions<{ withdraw: typeof withdraw; peek: typeof functions.peek }>> = {}) {
  const model = new Model();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  const client = createClient({
    functions: { withdraw, peek: functions.peek },
    schema,
    tenant: "acme",
    app: "bank",
    endpoint: "http://agent.test",
    token: "t0",
    fetch: modelFetch(model),
    sleep: () => Effect.void,
    ...over,
  });
  return { model, client };
}

Deno.test("read path 4: a handler that fails on a cached read is checked before the failure is believed", async () => {
  const { model, client } = bank();
  await run(client.mutation("peek", { a: "a1" })); // caches a1 = 10
  // The cache is right: the refusal stands, after one request that asserts the cached version.
  model.log.length = 0;
  const refused = await run(Effect.either(client.mutation("withdraw", { a: "a1", amount: 50 })));
  assert.equal(refused._tag, "Left");
  assert.deepEqual(model.log.map((r) => [r.kind, r.ops]), [
    ["query", [{ op: "assert", table: "accounts", id: "a1", expect: { version: 1 } }]],
  ]);
  // The cache is stale (the account was topped up behind it): the same call must not fail on old data.
  model.exec({
    v: 1,
    db: { tenant: "acme", app: "bank" },
    kind: "mutation",
    requestId: "top-up",
    schema: "",
    ops: [{ op: "patch", table: "accounts", id: "a1", value: { balance: 100 } }],
  });
  model.log.length = 0;
  assert.equal(await run(client.mutation("withdraw", { a: "a1", amount: 50 })), 50);
  // The assert of the cached version fails and answers the current account; the re-run commits.
  assert.deepEqual(kinds(model), ["query", "mutation"]);
  assert.equal(model.table("accounts").get("a1")!.balance, 50);
});

Deno.test("read path 4: a mutation that only reads never answers an unchecked cached document", async () => {
  const { model, client } = bank();
  await run(client.mutation("peek", { a: "a1" }));
  moveBehind(model, 4); // a1 = 6 at version 2; the cache still says 10
  model.log.length = 0;
  assert.deepEqual(await run(client.mutation("peek", { a: "a1" })), account("a1", 6, 2));
  assert.deepEqual(model.log.map((r) => r.ops), [[
    { op: "assert", table: "accounts", id: "a1", expect: { version: 1 } },
  ]]);
});

Deno.test("read path 4: queries do not read the cache; cache: false turns it off for mutations", async () => {
  const { model, client } = setup();
  model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  await run(client.query("pair", { a: "a1", b: "a2" }));
  await run(client.query("pair", { a: "a1", b: "a2" }));
  assert.deepEqual(kinds(model), ["query", "query"], "a query has no commit to prove a cached read");

  const off = setup(undefined, { cache: false });
  off.model.seed("accounts", [account("a1", 10), account("a2", 5)]);
  await run(off.client.mutation("transfer", { from: "a1", to: "a2", amount: 1 }));
  await run(off.client.mutation("transfer", { from: "a1", to: "a2", amount: 1 }));
  assert.deepEqual(kinds(off.model), ["query", "mutation", "query", "mutation"]);
});

Deno.test("read path 4: the cache keeps the newer version, forgets deletes, and is bounded", () => {
  const cache = memoryCache(2);
  cache.put("accounts", account("a1", 10, 3));
  cache.put("accounts", account("a1", 9, 2)); // an older answer arriving late
  assert.equal(cache.get("accounts", "a1")!.balance, 10);
  const given = cache.get("accounts", "a1")!;
  given.balance = 0; // what a caller does to its copy stays its own
  assert.equal(cache.get("accounts", "a1")!.balance, 10);
  cache.put("accounts", account("a2", 5));
  cache.get("accounts", "a1"); // a1 is now the more recently used
  cache.put("accounts", account("a3", 1));
  assert.equal(cache.get("accounts", "a2"), undefined, "the least recently used went");
  assert.ok(cache.get("accounts", "a1") && cache.get("accounts", "a3"));
  cache.delete("accounts", "a1");
  assert.equal(cache.get("accounts", "a1"), undefined);
});

// ---------------------------------------------------------------------------
// The sample app's makeMove, in round trips
// ---------------------------------------------------------------------------

Deno.test("read path: makeMove is two round trips cold and one warm; a stale cache costs the refused commit", async () => {
  const model = new Model();
  let ids = 0;
  const client = createClient({
    functions: app,
    schema: appSchema,
    tenant: "acme",
    app: "tic-tac-toe",
    endpoint: "http://agent.test",
    token: "t0",
    fetch: modelFetch(model),
    newId: () => `id-${++ids}`,
    sleep: () => Effect.void,
  });
  const { gameId } = await run(client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  const move = (playerId: string, cell: number, expectedVersion: number) =>
    run(Effect.either(client.mutation("makeMove", { gameId, playerId, cell, expectedVersion })));
  const trips = async (playerId: string, cell: number, expectedVersion: number) => {
    model.log.length = 0;
    const out = await move(playerId, cell, expectedVersion);
    return [out._tag, ...kinds(model)];
  };
  // Cold (an inserted document is not cached: its _creationTime is the server's to say): get, commit.
  assert.deepEqual(await trips("ada", 0, 1), ["Right", "query", "mutation"]);
  // Warm: the commit left the game in the cache at the version it wrote.
  assert.deepEqual(await trips("bob", 1, 2), ["Right", "mutation"]);
  assert.deepEqual(await trips("ada", 2, 3), ["Right", "mutation"]);
  // Another client moves; this one's cache is stale and its caller's expectedVersion is too. The handler's
  // own check passes on the cached version, the commit is refused, and the re-run (on the document the
  // refusal carried) raises the app's ConflictError: one trip, the refused commit, and no read.
  const doc = model.table("games").get(gameId)!;
  model.table("games").set(gameId, { ...doc, _version: doc._version + 1 });
  assert.deepEqual(await trips("bob", 3, 4), ["Left", "mutation"]);
  // With the right version the caller's retry is one trip again.
  assert.deepEqual(await trips("bob", 3, 5), ["Right", "mutation"]);
});
