# Learning path: customer

For someone who builds an application on the platform: you write a schema and handlers in TypeScript and call
them through the SDK. You do not need to read any Go. Work through this top to bottom. Each step is something
to run, read or try, followed by why it matters. Paths are from the repo root. Setup for every command:
`export PATH="$HOME/.deno/bin:$PATH"`.

## Phase 1: See what you get

1. Read the first screen of `README.md` (down to "Repository layout").

   It states the deal: you write schemas and document handlers, not SQL, and your handlers run in your own
   process. Everything you learn after this is detail on that.

2. Read `examples/tic-tac-toe/schema.ts`, `examples/tic-tac-toe/functions.ts` and
   `examples/tic-tac-toe/rules.ts`.

   This tic-tac-toe app is a complete example of what you will write. Notice there is no SQL, no connection
   and no transaction code anywhere in it.

3. On Linux, run `make platform-up` and read what it prints; leave it running. (On macOS skip to step 5: the
   script needs GNU tools and stops with `realpath: illegal option -- m`.)

   It starts a full local platform and proves it with one write and one read through the SDK. This is the
   quickest way to have something real to call. `make platform-down` removes it when you are done.

4. Read `scripts/platform/smoke.ts`.

   It is the shortest complete client program in the repo: create a client, call a mutation, call a query. It
   also plays four moves and counts round trips, which previews Phase 3.

## Phase 2: Write and call your own handlers

5. Follow `docs/guide.md` from "Installation" through "Configure and start agentd".

   One agentd with app tokens is the simplest server to develop against. Doing the setup by hand once shows
   you what a "database" is here: one SQLite file per tenant and app.

6. Do the "SDK walkthrough" in `docs/guide.md`: save the `guide.ts` module and run it.

   You create a game, read it, make a move, and deliberately break a rule. This is the full loop of an
   application call, including how an error comes back.

7. Read "Core concepts" in `docs/guide.md`.

   It defines a document (`_id`, `_creationTime`, `_version`), the two storage layouts, and what a schema
   fingerprint is. `SchemaDriftError` will make sense after this.

8. Read `sdk/schema.ts` and `sdk/functions.ts` (the exports and their comments).

   These are the two builders you use every day: `defineTable` with the `v.*` field types, and
   `defineFunctions` for `query` and `mutation`. Argument and result types are inferred from them.

9. Read "Query language and semantics" in `README.md`, then the public surface of `sdk/db.ts`.

   This is the complete list of what you can ask for: `get`, `find` with equality and `gt`, `sort`, `limit`,
   `count`, `countBy`, and the three writes. It also says plainly what is not supported (joins, optional
   fields, cursors), so you can design around it.

10. Add a handler of your own to `examples/tic-tac-toe/functions.ts` (for example a `resignGame` mutation or a
    `gamesByPlayer` query) and call it from `guide.ts`.

    Writing one handler teaches more than reading ten. If the types guide you to the right call without the
    docs, the SDK is doing its job.

11. Add a field or an index in `examples/tic-tac-toe/schema.ts`, call the old database, and read the error.

    You get a `RemoteError` with status 409 and `remoteTag: "SchemaDriftError"`: the server knows no schema
    with your new fingerprint for this app, and there is no automatic migration. Knowing this early shapes how
    you plan schema changes. Note the pattern: server errors reach you as `RemoteError`, and you tell them
    apart by `remoteTag`.

## Phase 3: The rules your handlers must follow

12. Read "App-facing client" in `README.md`, slowly.

    This is the contract between your handler and the platform: all reads of one run hold in one snapshot,
    writes are buffered, and one atomic commit carries them. It is the most important text for an app author.

13. Read the README's "Transaction and consistency contract" section.

    It says exactly what an acknowledged write means and what isolation you get. Rely on what this section
    promises and nothing more.

14. Learn the rule "handlers may run more than once": put a `console.log` in `makeMove`, create two clients,
    have both call `getGame` on one game (this warms both caches), then run both `makeMove` calls together
    with `Promise.all`.

    You will see three handler runs for two calls: the loser ran twice, the second time on the new state.

    When another writer changes something your handler read, the SDK re-runs the handler on the new state. So
    a handler must have no side effects beyond `db`: no emails, no HTTP calls, no counters.

15. Read the `find(...).unguarded()` paragraph in `docs/notes/intermediate-representation/read-path.md`
    ("Traps").

    A `find` in a mutation guards the whole table: any change to that table makes your commit retry. For a
    display-only read on a busy table, `unguarded()` avoids needless re-runs.

16. Run `deno test -A tests/unit/sdk_read_path_test.ts` and read the test names.

    Each test counts requests, so it shows what your code costs in round trips: batch reads with `db.getMany`
    or `Effect.all([...], { batching: true })`, and a warm mutation is one trip thanks to the client cache.

17. Note the limits: one commit holds at most 64 operations, and collection reads default to 100 results and
    cap at 1000.

    The 64 counts the asserts derived from your reads plus your writes, so a handler that reads first has
    fewer writes left. Past it the call fails locally (a `RemoteError` tagged `RuleError`) before anything is
    sent. Design large jobs as several calls.

## Phase 4: Errors, retries and credentials

18. Read `core/errors.ts` and the "Troubleshooting" section of `docs/guide.md`.

    Errors are typed and tagged, and troubleshooting maps each symptom to its cause and fix. Between them you
    can handle every failure your application will see.

19. Read `docs/retry-contract.md`.

    It is the table of what the SDK does for each answer: what it retries for you and what it gives up on. The
    key cases: `expired` is final, and no answer at all is retried but "may have applied". A `ConflictError`
    your own handler raises is not in the table: it is yours to handle (`sdk/client.ts`).

20. Stop agentd (`kill -9`), make a call from `guide.ts`, and look at the error you get.

    After about two seconds of retries you get `Unavailable` with `code: "transport"` and
    `mayHaveApplied: true` (the flag is conservative: it is set whenever nothing answered). Seeing it once
    tells you what to catch in production. The SDK's own retries of one commit keep its request id, so they
    apply at most once; a new call from your code gets a new id and is a new write.

21. Read "Authentication and tenancy" in `docs/guide.md`.

    It lists the credentials a single agentd accepts: a ticket, an app token, a tenant API key or a session.
    The credential decides the tenant. Multi-node deployments authenticate at controld instead, with a locator
    URL plus API key (next step).

22. Point `guide.ts` at the platform from step 3: replace `endpoint`/`token` with
    `locator: { url: "https://127.0.0.1:27080", apiKey, create: true, edge: "https://127.0.0.1:27443" }`, set
    `tenant` to your tenant id and `schema` to `withStorage(schema, "columns")`, and run it with
    `DENO_CERT=.platform/tls/ca.pem`. `scripts/platform/smoke.ts` shows the three things to do first: create a
    tenant, get an API key, and register the app's schema at controld.

    This is how a production client connects: it asks the locator once where the database lives, caches the
    answer and goes direct. Your handler code does not change at all.

## Phase 5: See what your app did

23. Pass a trace id as the third argument of a `query` or `mutation` call, then follow "Observability" in
    `docs/guide.md` to run `deno task logs:query trace --param traceId=<yours> --format json`.

    Every request of one call carries the same trace id, and with `trace_sql` on you can see the SQL your
    handler caused. This is the main tool for answering "why was that call slow?".

24. Run `deno task explain` and read its output.

    It prints the plan, the table and index DDL, and the compiled SQL for one sample `find` (games by status,
    newest first) in both layouts. To see your own query, change the `db.find(...)` line in
    `scripts/explain.ts` and run it again.

25. Open <https://nickstrad.github.io/mansio/> and read the `context`, `containers` and `caller-components`
    views.

    Now that you have used every part, the diagram shows where your process ends and the platform begins. If
    you want to go deeper, continue with the [developer](developer.md) path.

## Phase 6: Check yourself

26. Without notes, answer: two users call `makeMove` on the same game at the same moment. What does each one
    get back, and how many times did each handler run?

    A full answer uses the version precondition, the conflict answer with current state, and the re-run. If it
    is fuzzy, reread steps 12 to 14.

27. Without notes, answer: your mutation's call returned `Unavailable` with `mayHaveApplied: true`. What do
    you do?

    The write may or may not have committed, and calling again is a new write. Read the state and decide, or
    design the mutation so a second application is refused (as `makeMove` does with `expectedVersion`). This
    is the difference between a correct app and one that double-writes.
