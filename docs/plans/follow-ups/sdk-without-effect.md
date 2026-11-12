# Follow-up: an SDK that does not require Effect

Status: to investigate. Nothing here is decided or built. Written 2026-11-12.

## The question

Today a customer can **call** the platform without knowing [Effect](https://effect.website):
`createPromiseClient` returns promises. A customer cannot **write a handler** without it: a handler must
return an `Effect`, and every database call inside it (`db.get`, `db.insert`, `db.find(...).collect()`)
returns one too. Effect is a large, opinionated library. Requiring it to write the first handler is a cost for
every customer who does not already use it.

The goal to evaluate: the SDK works for someone who ignores Effect altogether (`async` handlers, `await`,
`try`/`catch`), and still works well for someone who uses Effect. Using Effect inside the SDK's own
implementation is fine either way.

## Where Effect is in the public API today

| Surface                                                  | What the customer sees                                                                 |
| -------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| Handlers (`sdk/functions.ts`)                            | `handler: (ctx, args) => Effect.Effect<R, E>`; no `async` form                         |
| The document API (`sdk/db.ts`)                           | Every read and write returns `Effect.Effect<T, DbError>`                               |
| Batching reads                                           | `db.getMany`, or `Effect.all([...], { batching: true })`                               |
| Handler failures                                         | `yield* new RuleError(...)`: errors are `Data.TaggedError` values in the error channel |
| Pure helpers (`examples/.../rules.ts`, `core/schema.ts`) | Return `Either`, which the handler yields                                              |
| The client (`sdk/client.ts`)                             | `createClient` returns Effects; `createPromiseClient` returns promises                 |
| Errors the caller catches                                | `RemoteError`, `Unavailable`: tagged errors that are also `Error` instances            |

Inside the SDK, Effect does four jobs worth naming, because each is something an `async` surface must keep:

1. **Read batching.** `sdk/recording.ts` models each read as an Effect request and resolves the ones asked for
   together in one round trip (`RequestResolver.makeBatched`). This is what makes a handler cheap.
2. **Re-running a handler.** On a conflict the client abandons the run and runs the handler again from the
   state the server returned. A handler is a description of work, so running it again is safe by construction.
3. **Typed failures.** A handler's error type lists what it can fail with, and the client's result type
   carries it to the caller.
4. **Interruption and deadlines.** A call's deadline can stop a run between steps.

## Options

### A. Keep Effect in handlers; hide it everywhere else (smallest step)

Promise client by default in every doc and example (done for the README and the guide on 2026-11-12). Handlers
stay Effects, with a short "you only need these four things" primer: `Effect.gen`, `yield*`, `db.getMany`,
failing with an error value.

- Cost: documentation only.
- Gives up: the goal. A customer still meets Effect in the first handler they write.

### B. Add an `async` handler form beside the Effect one

`defineFunctions` accepts either `handler: (ctx, args) => Effect<R, E>` or `handler: async (ctx, args) => R`.
The `async` form gets a promise-shaped `db` (`await db.get(...)`, `await db.find(...).collect()`,
`await db.getMany(...)`), and fails by throwing the SDK's error classes. Internally the SDK wraps the `async`
function in an Effect and the rest of the client is unchanged.

What has to be worked out:

- **Batching.** With `await`, two reads written one after the other are two round trips. Reads asked for
  together must still batch: `db.getMany`, and `Promise.all([db.get(a), db.get(b)])` if reads issued in the
  same tick are collected before the request is sent (the DataLoader technique). Decide whether same-tick
  collection is worth its subtlety or whether `getMany` alone is the documented way.
- **Re-runs.** An `async` function can be called again just as well, so the rule "no side effects beyond `db`"
  is unchanged. But abandoning a run mid-way is different: an Effect can be interrupted, a promise cannot. A
  stale run must be made harmless instead: its `db` stops answering (rejects), and its result is ignored.
  Check that nothing a stale run does can reach the server.
- **Errors.** Thrown errors are untyped in TypeScript. The caller still gets `RemoteError` with a tag at run
  time, but the compile-time list of a handler's failures is lost for `async` handlers. Decide whether that is
  acceptable (it is what most TypeScript users expect) and how a handler declares app errors
  (`throw new RuleError(...)`).
- **Pure helpers.** `core/schema.ts` and the example's `rules.ts` return `Either`. An `async` handler needs a
  plain alternative (throwing variants, or a small result type) so the example does not import Effect.
- **Deadlines.** Between `await`s the SDK cannot stop user code. The deadline is enforced where the SDK has
  control: before each request it sends and when the handler returns.
- **Two forms to maintain.** Types, tests and docs for both. The property tests in
  `tests/property/sdk_test.ts` and the request-counting tests in `tests/unit/sdk_read_path_test.ts` would run
  each scenario through both forms.

- Cost: medium. The client loop and the wire are untouched; the work is the promise-shaped `db`, the wrapper,
  the stale-run guard, and doubling the tests.
- Gives up: compile-time error lists for `async` handlers; interruption inside a handler.

### C. Make the plain form the only public API; Effect becomes an add-on

The SDK's public surface is promises and plain values. A separate entry point (for example `sdk/effect.ts`)
offers the Effect-shaped handler and client for customers who want them, built on the same core.

- Cost: high. It inverts the current design: the plain form is the base and the Effect form the adapter. Every
  public type changes, including the example app.
- Gives up: nothing permanent, but it is the largest change and the easiest to get subtly wrong (batching and
  re-runs must be rebuilt without Effect's request machinery, or that machinery stays as a hidden dependency).

### D. Remove Effect from the SDK entirely

Rewrite `sdk/` and the parts of `core/` it uses without the dependency.

- Cost: highest: about 2,200 lines of SDK plus the shared core, and their tests.
- Gains: a smaller install and no transitive dependency.
- Gives up: the Effect-native experience, unless option C's adapter is also kept. Not recommended unless
  bundle size or the dependency itself turns out to be a blocker for customers.

## What to find out before choosing

1. Who the first customers are: do they write TypeScript with Effect, or plain `async` code? One or two
   conversations answer this better than analysis.
2. A spike of option B on the example app: write `examples/tic-tac-toe/functions.ts` as `async` handlers and
   count round trips with the existing read-path tests. If `makeMove` stays at one trip warm and two cold,
   batching survived.
3. The stale-run guard: write the test first (a handler that keeps calling `db` after its run was abandoned
   must not produce a request), then see how small the guard can be.
4. What the SDK weighs when bundled for a browser or an edge function, with and without Effect.
5. Whether typed handler errors matter to anyone who is not already an Effect user.

## A first recommendation, to be tested

Option B, with `db.getMany` as the documented way to batch and same-tick collection only if the spike shows it
is simple. It reaches the goal (a customer can ignore Effect), keeps the Effect form for those who want it,
and leaves the client loop, the protocol and the server untouched. Revisit option C only if maintaining two
handler forms proves heavier than expected.

## Out of scope here

The server, the IR protocol and the retry contract do not change under any option: this is a question about
the shape of the TypeScript API only.
