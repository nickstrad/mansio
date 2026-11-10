/**
 * B8: the SDK is held to the caller contract by the shared vectors (`ir/vectors/retry.json`, generated from
 * mansio-server's `front.Contract`; the Go client runs the same file in `internal/client`). For each answer
 * from an agent or from controld's resolve: the step the SDK takes, whether a mutation that ends there may
 * have applied, and the default limits. Then, through the real transport: a reroute after a call's first
 * spends the budget, and the first waits a jittered delay.
 */
import assert from "node:assert/strict";
import { Effect, Either } from "effect";
import { Unavailable } from "../../core/errors.ts";
import { functions } from "../../app/functions.ts";
import { schema } from "../../app/schema.ts";
import { createClient } from "../../sdk/client.ts";
import {
  decide,
  DEFAULT_MAX_REROUTES,
  DEFAULT_RETRY,
  MAX_RETRY_AFTER_MS,
  mayHaveApplied,
  resolveStep,
  retryAfterMs,
  RetryBudget,
} from "../../sdk/retry.ts";

interface Vector {
  readonly name: string;
  readonly from: "agent" | "resolve";
  readonly code: string;
  readonly status: number;
  readonly retryAfter: boolean;
  readonly action: "done" | "retry" | "reroute" | "fail";
  readonly budget: "" | "every" | "after_first";
  readonly mayHaveApplied: boolean;
}

const file = JSON.parse(await Deno.readTextFile(new URL("../../ir/vectors/retry.json", import.meta.url))) as {
  readonly policy: Record<string, number>;
  readonly vectors: readonly Vector[];
};

Deno.test("retry contract: the SDK's defaults are the contract's", () => {
  const budget = new RetryBudget();
  assert.deepEqual({
    maxAttempts: DEFAULT_RETRY.maxAttempts,
    maxReroutes: DEFAULT_MAX_REROUTES,
    backoffBaseMs: DEFAULT_RETRY.baseMs,
    backoffMaxMs: DEFAULT_RETRY.maxMs,
    maxRetryAfterMs: MAX_RETRY_AFTER_MS,
    budgetRatio: budget.ratio,
    budgetBurst: budget.burst,
  }, file.policy);
  assert.equal(retryAfterMs("3600"), file.policy.maxRetryAfterMs, "a longer hint is capped");
});

Deno.test("retry contract: every vector's step, budget and may-have-applied", () => {
  assert.ok(file.vectors.length >= 25, `only ${file.vectors.length} vectors`);
  for (const v of file.vectors) {
    if (v.from === "resolve" && v.code === "ok") continue; // a route: nothing to decide
    const step = v.from === "agent" ? decide(v.code) : resolveStep(v.code);
    assert.equal(step, v.action, v.name);
    // The transport charges every retry and every reroute after a call's first (sdk/transport.ts).
    const charged = step === "retry" ? "every" : step === "reroute" ? "after_first" : "";
    assert.equal(charged, v.budget, `${v.name}: budget`);
    // The SDK reports may-have-applied on Unavailable only: for the answers that are not a server-decided
    // failure (RemoteError). `internal` reaches the SDK as a 5xx RemoteError, which carries no such flag.
    if (v.from === "agent" && v.code !== "ok" && v.code !== "internal") {
      assert.equal(mayHaveApplied(v.code), v.mayHaveApplied, `${v.name}: mayHaveApplied`);
    }
  }
});

/** A locator-routed client whose agents always answer `wrong_owner`: every attempt ends in a reroute. */
function alwaysMoved(budget: RetryBudget) {
  const sleeps: number[] = [];
  let resolves = 0, sends = 0;
  const fetchFn: typeof fetch = ((url: string) => {
    if (url === "http://controld.test/v1/resolve") {
      const epoch = ++resolves;
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
    sends++;
    return Promise.resolve(
      Response.json({ error: { tag: "wrong_owner", message: "moved" } }, { status: 421 }),
    );
  }) as typeof fetch;
  const client = createClient({
    functions,
    schema,
    tenant: "acme",
    app: "tic-tac-toe",
    locator: { url: "http://controld.test", apiKey: "mansio_k" },
    fetch: fetchFn,
    budget,
    random: () => 0.5,
    sleep: (ms) => Effect.sync(() => void sleeps.push(ms)),
  });
  return { client, sleeps, counts: () => ({ resolves, sends }) };
}

Deno.test("retry contract: the first reroute is free and jittered; later ones spend the budget", async () => {
  // One token: the first reroute is free, the second takes the token, the third is refused.
  const one = alwaysMoved(new RetryBudget(0.01, 1));
  const out = await Effect.runPromise(Effect.either(one.client.query("getGame", { gameId: "g1" })));
  assert.ok(Either.isLeft(out) && out.left instanceof Unavailable);
  assert.equal(out.left.code, "wrong_owner");
  assert.deepEqual(one.counts(), { resolves: 3, sends: 3 }, "before B8: 6 of each, every reroute free");
  // random() = 0.5: half of the backoff ceiling, 50 ms then 100 ms. Before B8 the first wait was 0.
  assert.deepEqual(one.sleeps, [25, 50]);

  // With the default budget (10 banked tokens) the reroute cap still ends the call.
  const many = alwaysMoved(new RetryBudget());
  await Effect.runPromise(Effect.either(many.client.query("getGame", { gameId: "g1" })));
  assert.deepEqual(many.counts(), { resolves: DEFAULT_MAX_REROUTES + 1, sends: DEFAULT_MAX_REROUTES + 1 });
});
