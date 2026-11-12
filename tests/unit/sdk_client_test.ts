/**
 * The IR-3 SDK client against an in-memory `/v1/ops` (`sdk_model.ts`), through the real HTTP transport
 * (a fake `fetch`): what the sample app's handlers send, the handler-retry model (derived vs app
 * conflicts, duplicates, lost replies, shed, budget), routing (static tokens, the locator), and the
 * ticket and id formats. Pure: no server, no SQLite.
 */
import assert from "node:assert/strict";
import { Effect, Either } from "effect";
import { ConflictError, RemoteError, Unavailable } from "../../core/errors.ts";
import type { Request } from "../../core/intermediate-representation/types.ts";
import { functions } from "../../examples/tic-tac-toe/functions.ts";
import { schema } from "../../examples/tic-tac-toe/schema.ts";
import {
  type ClientOptions,
  createClient,
  createPromiseClient,
  schemaFingerprint,
} from "../../sdk/client.ts";
import { uuidv7Generator } from "../../sdk/ids.ts";
import { RetryBudget } from "../../sdk/retry.ts";
import { encodeClaims, importIssuerSeed, mintTicket, PERM_READ, PERM_WRITE } from "../../sdk/ticket.ts";
import { type Doc, type Fault, Model, modelFetch } from "./sdk_model.ts";

const UUID_V7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function counter(prefix: string) {
  let n = 0;
  return () => `${prefix}${++n}`;
}

const game = (id: string, version: number, over: Record<string, unknown> = {}): Doc => ({
  _id: id,
  _creationTime: 500,
  _version: version,
  xPlayer: "ada",
  oPlayer: "bob",
  board: Array(9).fill(null),
  turn: "X",
  status: "playing",
  winner: null,
  ...over,
});

function setup(
  fault?: (request: Request, n: number) => Fault | undefined,
  over: Partial<ClientOptions<typeof functions>> = {},
) {
  const model = new Model();
  const seen = { headers: [] as Headers[] };
  const sleeps: number[] = [];
  const client = createClient({
    functions,
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    endpoint: "http://agent.test",
    token: "t0",
    fetch: modelFetch(model, fault, seen),
    newId: counter("id-"),
    newRequestId: counter("req-"),
    now: () => 1000,
    random: () => 0.5,
    sleep: (ms) => Effect.sync(() => void sleeps.push(ms)),
    ...over,
  });
  return { model, client, seen, sleeps };
}

const either = <A, E>(effect: Effect.Effect<A, E>) => Effect.runPromise(Effect.either(effect));

Deno.test("sdk: createGame is one mutation with a client id and no reads", async () => {
  const { model, client } = setup();
  const out = await Effect.runPromise(client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.deepEqual(out, { gameId: "id-1" });
  assert.equal(model.log.length, 1);
  const [request] = model.log;
  assert.equal(request.kind, "mutation");
  assert.equal(request.requestId, "req-1");
  assert.equal(request.schema, await schemaFingerprint(schema));
  assert.deepEqual(request.db, { tenant: "acme", app: "tic-tac-toe" });
  assert.deepEqual(request.ops, [{
    op: "insert",
    table: "games",
    id: "id-1",
    value: {
      xPlayer: "ada",
      oPlayer: "bob",
      board: Array(9).fill(null),
      turn: "X",
      status: "playing",
      winner: null,
    },
  }]);
});

Deno.test("sdk: makeMove reads once, asserts the version it read, and answers its own write back", async () => {
  const { model, client } = setup();
  model.seed("games", [game("g1", 3)]);
  const out = await Effect.runPromise(
    client.mutation("makeMove", { gameId: "g1", playerId: "ada", cell: 4, expectedVersion: 3 }),
  );
  const board = Array(9).fill(null);
  board[4] = "X";
  // The handler's final get answers the local state: the patch applied, the version the server will set.
  assert.deepEqual(out, game("g1", 4, { board, turn: "O" }));
  assert.deepEqual(model.table("games").get("g1"), out);
  assert.deepEqual(model.log.map((r) => r.kind), ["query", "mutation"]);
  assert.deepEqual(model.log[0].ops, [{ op: "get", table: "games", id: "g1" }]);
  assert.deepEqual(model.log[1].ops, [
    { op: "assert", table: "games", id: "g1", expect: { version: 3 } },
    {
      op: "patch",
      table: "games",
      id: "g1",
      value: { xPlayer: "ada", oPlayer: "bob", board, turn: "O", status: "playing", winner: null },
    },
    {
      op: "insert",
      table: "moves",
      id: "id-1",
      value: { gameId: "g1", playerId: "ada", mark: "X", cell: 4, moveNumber: 1 },
    },
  ]);
});

Deno.test("sdk: deleteGame's find sees nothing it deleted; a missing game fails locally without a commit", async () => {
  const { model, client } = setup();
  model.seed("games", [game("g1", 2)]);
  model.seed("moves", [
    {
      _id: "m1",
      _creationTime: 1,
      _version: 1,
      gameId: "g1",
      playerId: "ada",
      mark: "X",
      cell: 0,
      moveNumber: 1,
    },
    {
      _id: "m2",
      _creationTime: 2,
      _version: 1,
      gameId: "g2",
      playerId: "ada",
      mark: "X",
      cell: 0,
      moveNumber: 1,
    },
  ]);
  assert.deepEqual(await Effect.runPromise(client.mutation("deleteGame", { gameId: "g1" })), {
    deleted: true,
  });
  assert.deepEqual([...model.table("moves").keys()], ["m2"]);
  assert.deepEqual(model.log.at(-1)!.ops, [
    { op: "assert", table: "games", id: "g1", expect: { version: 2 } },
    { op: "assert", table: "moves", expect: { unchangedSince: 0 } },
    { op: "delete", table: "moves", id: "m1" },
    { op: "delete", table: "games", id: "g1" },
  ]);
  const missing = await either(client.mutation("deleteGame", { gameId: "g1" }));
  assert.ok(Either.isLeft(missing) && missing.left instanceof RemoteError);
  assert.equal(missing.left.status, 404);
  assert.equal(missing.left.remoteTag, "NotFoundError");
  assert.equal(model.log.at(-1)!.kind, "query", "a handler that fails sends no commit");
});

Deno.test("sdk: an app-raised ConflictError goes to the caller, unretried; argument and name errors are local", async () => {
  const { model, client } = setup();
  model.seed("games", [game("g1", 3)]);
  const stale = await either(
    client.mutation("makeMove", { gameId: "g1", playerId: "ada", cell: 4, expectedVersion: 1 }),
  );
  assert.ok(Either.isLeft(stale) && stale.left instanceof RemoteError);
  assert.equal(stale.left.status, 409);
  assert.equal(stale.left.remoteTag, "ConflictError");
  assert.ok(stale.left.cause instanceof ConflictError);
  assert.equal(stale.left.message, "Version conflict: expected 1, found 3");
  assert.deepEqual(model.log.map((r) => r.kind), ["query"]);

  const bad = await either(client.mutation("createGame", { xPlayer: 42 as unknown as string, oPlayer: "b" }));
  assert.ok(Either.isLeft(bad) && bad.left instanceof RemoteError);
  assert.deepEqual([bad.left.status, bad.left.remoteTag, bad.left.message], [
    400,
    "SchemaError",
    "xPlayer: expected string",
  ]);
  const rule = await either(client.mutation("createGame", { xPlayer: "a", oPlayer: "a" }));
  assert.ok(Either.isLeft(rule) && rule.left instanceof RemoteError);
  assert.deepEqual([rule.left.status, rule.left.remoteTag], [400, "RuleError"]);
  const unknown = await either(client.query("nope" as "getGame", { gameId: "g1" }));
  assert.ok(Either.isLeft(unknown) && unknown.left instanceof RemoteError);
  assert.deepEqual([unknown.left.status, unknown.left.remoteTag], [404, "UnknownHandlerError"]);
  const kind = await either(client.query("createGame" as "getGame", { gameId: "g1" }));
  assert.ok(Either.isLeft(kind) && kind.left instanceof RemoteError);
  assert.deepEqual([kind.left.status, kind.left.remoteTag], [400, "HandlerKindMismatch"]);
  assert.equal(model.log.length, 1, "none of these reached the server");
});

Deno.test("sdk: a derived conflict re-runs the handler on the state the conflict carried", async () => {
  let bumped = false;
  const { model, client, sleeps } = setup((request) => {
    if (request.kind !== "mutation" || bumped) return undefined;
    bumped = true; // someone else moves first, between this handler's read and its commit
    return { kind: "before", run: () => model.exec(concurrentMove) };
  });
  const concurrentMove: Request = {
    v: 1,
    db: { tenant: "acme", app: "tic-tac-toe" },
    kind: "mutation",
    requestId: "other",
    schema: "x",
    ops: [{ op: "patch", table: "games", id: "g1", value: { status: "playing" } }],
  };
  model.seed("games", [game("g1", 3)]);
  // The game moved on to version 4 under us: makeMove's own expectedVersion check now refuses, locally.
  const out = await either(
    client.mutation("makeMove", { gameId: "g1", playerId: "ada", cell: 4, expectedVersion: 3 }),
  );
  assert.ok(Either.isLeft(out) && out.left instanceof RemoteError && out.left.cause instanceof ConflictError);
  const mutations = model.log.filter((r) => r.kind === "mutation" && r.requestId !== "other");
  assert.equal(mutations.length, 1, "the second run failed in the handler, before any commit");
  assert.equal(sleeps.length, 1, "one jittered pause between the runs");
  assert.deepEqual(
    model.log.filter((r) => r.kind === "query").length,
    1,
    "the re-run took the game from the conflict instead of reading it again",
  );
});

Deno.test("sdk: derived conflicts retry until they converge, each run under a new request id", async () => {
  const model = new Model();
  model.seed("games", [game("g1", 1)]);
  let concurrent = 2;
  const client = setup(undefined, {
    fetch: modelFetch(model, (request) => {
      if (request.kind !== "mutation" || concurrent === 0) return undefined;
      concurrent--;
      return {
        kind: "before",
        run: () => {
          const doc = model.table("games").get("g1")!;
          model.table("games").set("g1", { ...doc, _version: doc._version + 1 });
        },
      };
    }),
  }).client;
  // deleteGame has no expectedVersion of its own: only the derived assert guards it.
  assert.deepEqual(await Effect.runPromise(client.mutation("deleteGame", { gameId: "g1" })), {
    deleted: true,
  });
  const commits = model.log.filter((r) => r.kind === "mutation");
  assert.deepEqual(commits.map((r) => r.requestId), ["req-1", "req-2", "req-3"]);
  assert.deepEqual(
    commits.map((r) => r.ops[0]),
    [1, 2, 3].map((version) => ({
      op: "assert",
      table: "games",
      id: "g1",
      expect: { version },
    })),
  );
  assert.equal(model.table("games").size, 0);
});

Deno.test("sdk: derived conflicts past handlerRetries reach the caller as the ConflictError at op 0", async () => {
  const model = new Model();
  model.seed("games", [game("g1", 1)]);
  const client = setup(undefined, {
    handlerRetries: 2,
    fetch: modelFetch(model, (request) =>
      request.kind === "mutation"
        ? {
          kind: "before",
          run: () => {
            const doc = model.table("games").get("g1")!;
            model.table("games").set("g1", { ...doc, _version: doc._version + 1 });
          },
        }
        : undefined),
  }).client;
  const out = await either(client.mutation("deleteGame", { gameId: "g1" }));
  assert.ok(Either.isLeft(out) && out.left instanceof RemoteError);
  assert.deepEqual([out.left.status, out.left.remoteTag, out.left.op, out.left.fields?.check], [
    409,
    "ConflictError",
    0,
    "version",
  ]);
  assert.equal(model.log.filter((r) => r.kind === "mutation").length, 3);
});

Deno.test("sdk: a lost reply is retried with the same body and answered as a duplicate, applied once", async () => {
  const { model, client, seen } = setup((request, n) =>
    request.kind === "mutation" && n === 0 ? { kind: "lost" } : undefined
  );
  const out = await Effect.runPromise(
    client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }, "trace-1"),
  );
  assert.deepEqual(out, { gameId: "id-1" });
  assert.deepEqual(model.log.map((r) => r.requestId), ["req-1", "req-1"]);
  assert.equal(model.table("games").size, 1);
  assert.deepEqual(seen.headers.map((h) => h.get("x-trace-id")), ["trace-1", "trace-1"]);
  for (const h of seen.headers) {
    assert.equal(h.get("authorization"), "Bearer t0");
    assert.ok(Number(h.get("x-timeout-ms")) >= 1);
    assert.equal(h.get("x-mansio-handler"), "createGame", "the event log's handler name");
  }
});

Deno.test("sdk: shed honors Retry-After; aborted retries; expired is final; the budget and attempts bound it", async () => {
  const shed = setup((_r, n) => n === 0 ? { kind: "shed", retryAfter: "2" } : undefined);
  await Effect.runPromise(shed.client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.deepEqual(shed.sleeps, [2000]);

  const flaky = setup((_r, n) => n === 0 ? { kind: "aborted" } : n === 1 ? { kind: "shed" } : undefined);
  await Effect.runPromise(flaky.client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.equal(flaky.model.table("games").size, 1);
  assert.deepEqual(
    flaky.model.log.map((r) => r.requestId),
    ["req-1"],
    "only the attempt that got through ran",
  );

  // B8: expired is not retried (its deadline is spent). The caller gets Unavailable and, for a mutation,
  // is told it may have applied.
  const late = setup(() => ({ kind: "expired" }));
  const expired = await either(late.client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.ok(Either.isLeft(expired) && expired.left instanceof Unavailable);
  assert.deepEqual([expired.left.code, expired.left.mayHaveApplied], ["expired", true]);
  assert.equal(late.sleeps.length, 0, "no retry of an expired request");

  const always = setup(() => ({ kind: "shed" }));
  const out = await either(always.client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.ok(Either.isLeft(out) && out.left instanceof Unavailable);
  assert.deepEqual([out.left.code, out.left.mayHaveApplied], ["shed", false]);
  assert.equal(always.sleeps.length, 2, "3 attempts by default");

  const broke = setup(() => ({ kind: "shed" }), { budget: new RetryBudget(0.1, 0) });
  const spent = await either(broke.client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.ok(Either.isLeft(spent) && spent.left instanceof Unavailable);
  assert.equal(broke.sleeps.length, 0, "an empty budget allows no retry");

  const gone = setup(() => ({ kind: "lost" }));
  const lost = await either(gone.client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.ok(Either.isLeft(lost) && lost.left instanceof Unavailable);
  assert.deepEqual([lost.left.code, lost.left.mayHaveApplied], ["transport", true]);
  assert.equal(gone.model.table("games").size, 1, "applied once whatever the replies did");
});

Deno.test("sdk: a server-decided failure on the commit is final (no re-run)", async () => {
  const model = new Model();
  // A blind patch of a missing document: the handler did not read it, so the server decides.
  const blind = createClient({
    functions: {
      touch: (await import("../../sdk/functions.ts")).defineFunctions(schema).mutation({
        args: {},
        handler: ({ db }) => db.patch("games", "nope", { status: "won" }),
      }),
    },
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    endpoint: "http://agent.test",
    token: "t",
    fetch: modelFetch(model),
    newRequestId: counter("r-"),
  });
  const out = await either(blind.mutation("touch", {}));
  assert.ok(Either.isLeft(out) && out.left instanceof RemoteError);
  assert.deepEqual([out.left.status, out.left.remoteTag, out.left.op], [404, "NotFoundError", 0]);
  assert.equal(model.log.length, 1);
});

Deno.test("sdk: a token function is asked per kind and again after ticket_invalid; wrong_owner is final on a static route", async () => {
  const model = new Model();
  model.seed("games", [game("g1", 1)]);
  const asked: string[] = [];
  let refuse = true;
  const fetchFn: typeof fetch = ((url: string, init: RequestInit) => {
    const auth = new Headers(init.headers).get("authorization")!;
    if (auth === "Bearer query-1" && refuse) {
      refuse = false;
      return new Response(JSON.stringify({ error: { tag: "ticket_invalid", message: "expired" } }), {
        status: 401,
      });
    }
    return modelFetch(model)(url, init);
  }) as typeof fetch;
  const client = createClient({
    functions,
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    endpoint: "http://agent.test/",
    token: (kind) => {
      asked.push(kind);
      return `${kind}-${asked.length}`;
    },
    fetch: fetchFn,
    sleep: () => Effect.void,
  });
  assert.equal((await Effect.runPromise(client.query("getGame", { gameId: "g1" })))?._id, "g1");
  assert.deepEqual(asked, ["query", "query"]);
  await Effect.runPromise(client.query("getGame", { gameId: "g1" }));
  assert.deepEqual(asked, ["query", "query"], "cached");

  const owner = createClient({
    functions,
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    endpoint: "http://agent.test",
    token: "t",
    fetch: (() =>
      Promise.resolve(
        new Response(JSON.stringify({ error: { tag: "wrong_owner", message: "moved" } }), { status: 421 }),
      )) as typeof fetch,
  });
  const moved = await either(owner.query("getGame", { gameId: "g1" }));
  assert.ok(Either.isLeft(moved) && moved.left instanceof Unavailable);
  assert.equal(moved.left.code, "wrong_owner");
});

Deno.test("sdk: the locator route re-resolves with min_epoch after wrong_owner and caches the route", async () => {
  const model = new Model();
  model.seed("games", [game("g1", 1)]);
  const resolves: unknown[] = [];
  const fetchFn: typeof fetch = ((url: string, init: RequestInit) => {
    if (url === "http://controld.test/v1/resolve") {
      const body = JSON.parse(String(init.body));
      resolves.push(body);
      assert.equal(new Headers(init.headers).get("authorization"), "Bearer mansio_k");
      const epoch = body.min_epoch ?? 1;
      return Promise.resolve(Response.json({
        code: "ok",
        db: "acme:tic-tac-toe",
        vm: `vm-${epoch}`,
        endpoint: `agent-${epoch}.test:7070`,
        epoch,
        ticket: `ticket-${epoch}`,
        ttl_ms: 60_000,
      }));
    }
    if (url.startsWith("http://agent-1.test:7070/")) {
      return Promise.resolve(
        Response.json({ error: { tag: "wrong_owner", message: "moved" } }, { status: 421 }),
      );
    }
    assert.equal(url, "http://agent-2.test:7070/v1/ops");
    assert.equal(new Headers(init.headers).get("authorization"), "Bearer ticket-2");
    return modelFetch(model)(url, init);
  }) as typeof fetch;
  const client = createClient({
    functions,
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    locator: { url: "http://controld.test", apiKey: "mansio_k", create: true },
    fetch: fetchFn,
    sleep: () => Effect.void,
  });
  assert.equal((await Effect.runPromise(client.query("getGame", { gameId: "g1" })))?._id, "g1");
  await Effect.runPromise(client.query("getGame", { gameId: "g1" }));
  assert.deepEqual(resolves, [
    { tenant: "acme", app: "tic-tac-toe", create: true },
    { tenant: "acme", app: "tic-tac-toe", create: true, min_epoch: 2 },
  ]);
});

Deno.test("sdk: with an edge, the locator route sends every request to the edge with the ticket", async () => {
  const model = new Model();
  model.seed("games", [game("g1", 1)]);
  const seen: string[] = [];
  const fetchFn: typeof fetch = ((url: string, init: RequestInit) => {
    if (url === "http://controld.test/v1/resolve") {
      return Promise.resolve(Response.json({
        code: "ok",
        db: "acme:tic-tac-toe",
        vm: "vm-a",
        endpoint: "agent-a.test:7070",
        epoch: 1,
        ticket: "ticket-a",
        ttl_ms: 60_000,
      }));
    }
    seen.push(`${url} ${new Headers(init.headers).get("authorization")}`);
    return modelFetch(model)(url, init);
  }) as typeof fetch;
  const client = createClient({
    functions,
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    locator: { url: "http://controld.test", apiKey: "mansio_k", edge: "https://edge.test/" },
    fetch: fetchFn,
    sleep: () => Effect.void,
  });
  assert.equal((await Effect.runPromise(client.query("getGame", { gameId: "g1" })))?._id, "g1");
  assert.deepEqual(seen, ["https://edge.test/v1/ops Bearer ticket-a"]);
});

Deno.test("sdk: createPromiseClient rejects with the ClientError itself", async () => {
  const model = new Model();
  const client = createPromiseClient({
    functions,
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    endpoint: "http://agent.test",
    token: "t",
    fetch: modelFetch(model),
  });
  assert.equal(await client.query("getGame", { gameId: "none" }), null);
  await assert.rejects(
    client.mutation("makeMove", { gameId: "none", playerId: "a", cell: 0, expectedVersion: 1 }),
    (e) => e instanceof RemoteError && e.status === 404,
  );
});

Deno.test("sdk: tickets are byte-identical to mansio-server's internal/ticket Mint", async () => {
  // Golden from `ticket.Mint` with seed bytes 0..31 (generated once with a throwaway Go test).
  const key = await importIssuerSeed(Uint8Array.from({ length: 32 }, (_, i) => i));
  assert.equal(
    await mintTicket(key, {
      kid: "k1",
      tid: "local",
      dbid: "local:tic-tac-toe",
      vm: "vm-local",
      epoch: 1,
      sub: "e2e",
      perms: PERM_READ | PERM_WRITE,
      iat: 1762300000,
      exp: 1762300600,
    }),
    "AQJrMQVsb2NhbBFsb2NhbDp0aWMtdGFjLXRvZQh2bS1sb2NhbAEAA2UyZQPgoKrIBrilqsgGHGSfNsbk3XFpGeejZOn4EY6o_bJbFUsYJNQmrHb-tmZ6PoXwIapVYjEr8vRt9CB68Nf8m4b5L9n1E9kR92tCDw",
  );
  assert.equal(
    await mintTicket(key, {
      kid: "key-two",
      tid: "acme",
      dbid: "acme:notes",
      vm: "vm-7",
      epoch: 300,
      gen: 129,
      sub: "svc",
      perms: PERM_READ,
      iat: 0,
      exp: 2 ** 35,
    }),
    "AQdrZXktdHdvBGFjbWUKYWNtZTpub3RlcwR2bS03rAKBAQNzdmMBAICAgICAAYM7Hx0Kka7i9yJNm3xNkaZAPK1KLIE13Z7L5cDDBFFT0AuVP6Fnm9gHrjuTqBTcLSMYumsjhFHEG1bEUSp2egk",
  );
  assert.throws(() =>
    encodeClaims({ kid: "", tid: "t", dbid: "d", vm: "v", epoch: 1, sub: "s", perms: 1, iat: 0, exp: 1 })
  );
});

Deno.test("sdk: UUIDv7 ids are canonical and ordered, within a millisecond and across a clock step back", () => {
  let t = 1_700_000_000_000;
  const next = uuidv7Generator(() => t);
  const ids = [next(), next(), next()];
  t -= 5;
  ids.push(next());
  t += 100;
  ids.push(next());
  for (const id of ids) assert.match(id, UUID_V7);
  assert.deepEqual([...ids].sort(), ids);
  assert.equal(new Set(ids).size, ids.length);
});

Deno.test("sdk: a find whose widened limit hits MAX_LIMIT is exact or refused loudly; > 64 ops fail locally", async () => {
  const model = new Model();
  model.seed(
    "moves",
    Array.from({ length: 1005 }, (_, i) => ({
      _id: `d${String(i).padStart(5, "0")}`,
      _creationTime: 1,
      _version: 1,
      gameId: "g",
      playerId: "p",
      mark: "X",
      cell: 0,
      moveNumber: i,
    })),
  );
  const { mutation } = (await import("../../sdk/functions.ts")).defineFunctions(schema);
  const client = createClient({
    functions: {
      trim: mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            yield* db.delete("moves", "d00000");
            return (yield* db.find("moves").limit(1000).collect()).map((m) => m._id);
          }),
      }),
      wide: mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            for (let i = 0; i < 10; i++) {
              yield* db.patch("moves", `d${String(i).padStart(5, "0")}`, { cell: 1 });
            }
            return (yield* db.find("moves").sort("moveNumber", "desc").limit(1000).collect()).length;
          }),
      }),
      many: mutation({
        args: {},
        handler: ({ db }) =>
          Effect.gen(function* () {
            for (let i = 0; i < 65; i++) yield* db.insert("games", game("x", 1) as never);
          }),
      }),
    },
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    endpoint: "http://agent.test",
    token: "t",
    fetch: modelFetch(model),
  });
  // Delete one of 1005, then find().limit(1000): the server can answer at most 1000 rows and there is no
  // cursor, so the 1000th untouched row is unknown. Refused, never answered short (was 999 rows).
  const trim = await either(client.mutation("trim", {}));
  assert.ok(Either.isLeft(trim) && trim.left instanceof RemoteError);
  assert.deepEqual([trim.left.status, trim.left.remoteTag], [400, "RuleError"]);
  assert.match(trim.left.message, /cannot be answered exactly/);
  // Descending with 10 patched: the 1000 highest are untouched rows plus the cap check holds.
  assert.equal(await Effect.runPromise(client.mutation("wide", {})), 1000);
  const many = await either(client.mutation("many", {}));
  assert.ok(Either.isLeft(many) && many.left instanceof RemoteError);
  assert.deepEqual([many.left.status, many.left.remoteTag], [400, "RuleError"]);
  assert.match(many.left.message, /65 operations/);
});

Deno.test("sdk: nothing is sent once the call's deadline has passed", async () => {
  // The first reading sets the deadline (0 + 10 ms); every later one is past it.
  let readings = 0;
  const late = setup(undefined, { now: () => (readings++ === 0 ? 0 : 1_000), timeoutMs: 10 });
  const result = await either(late.client.mutation("createGame", { xPlayer: "ada", oPlayer: "bob" }));
  assert.ok(Either.isLeft(result) && result.left instanceof Unavailable);
  assert.deepEqual([result.left.code, result.left.mayHaveApplied], ["deadline", false]);
  assert.equal(late.model.log.length, 0);
});
