# The colocated runner, in-agentd execution, and the sandbox choice: survey changes 5, 6 and 7

Date: 2026-11-10 · Branch: `feat/read-path-1234` · For Nick to rule on later. No code for changes 5, 6 or 7
exists or was written for this document.

Source: the survey `docs/programmatic-schema-platform-survey.md` (branch `docs/programmatic-schema-survey`),
§3.2 changes 5 to 7. Statements about mansio are from the code on this branch, after changes 1 to 4.
Statements about other products and runtimes (V8, QuickJS, wazero, goja, Replicache) are from knowledge and
were not checked against their documentation; where a design depends on one, the text says what a spike must
confirm.

## The short version

- **Change 5** adds a process, the **runner**, that runs an app's handlers next to agentd on behalf of callers
  that are far away or cannot be trusted with a write ticket. It is the SDK you already have, behind an HTTP
  listener, with authentication in front. agentd does not change and still runs no app code.
- **Change 6** moves the handler **into agentd**, inside the database's write transaction. One round trip, no
  conflicts, and mansio becomes a platform that runs customer code in its own server process.
- **Change 7** is not a third place to run code. It is the question "which sandbox?", and it only exists if
  change 6 is adopted.

The line between 5 and 6 is whose process the handler runs in. In 5 it is the app owner's process, and a bug
or an exploit there can reach what that app's key can reach. In 6 it is mansio's process, and the same bug
shares an address space with every tenant's database on that VM.

## Where the code stands (after changes 1 to 4)

A handler runs in the caller's process, inside the SDK's recording executor (`sdk/recording.ts`). What matters
for the rest of this document:

- The executor reaches the outside through one function, `Reads`, which answers reads with the commit sequence
  they were read at. It knows no transport.
- All reads of one run hold in one snapshot. Reads asked for together are one request.
- The commit carries a precondition for every read (`get`: the version; `find`: the table unchanged since the
  read). A failed precondition answers the current state, and the handler runs again on it without reading
  again.
- A mutation's `get` is answered from a client-side cache when the cache holds the document, and the cached
  version is asserted. `makeMove` is one round trip when the cache is warm, two when it is cold and two when
  the cache is stale (measured through the edge by `make platform-up`'s smoke); a commit conflict costs one
  more trip, not the reads again (pinned by `tests/unit/sdk_read_path_test.ts`).

So for a caller placed next to agentd the read path is no longer the problem. What remains is the caller that
is not next to agentd, and the caller that must not hold a write ticket. That is change 5's subject.

## Change 5: the colocated function runner

### What it is

A long-running process, `mansio serve`, that:

1. loads the app's `functions` object and schema (the same values `createClient` takes today);
2. listens for HTTP calls, one route per handler;
3. authenticates each call, turns the caller's identity into a tenant, and runs the handler with the existing
   SDK client against agentd;
4. answers the handler's result, or its error in the wire form `/v1/ops` already uses.

It is application-tier code: the app owner's handlers plus a host that mansio ships. Until IR-6 the Deno
platform's handler routes (`POST /apps/:app/(query|mutation)/:name`) did this job; the runner is those routes
without a database inside them. The Go plan named it as the fallback when it chose not to embed an engine
(`docs/plans/completed/replace-platform-with-go.md`, decision D2: "Deno sidecar, optimistic commit").

### Who deploys and owns it

The app owner. They build it from their repository (handlers, schema, the mansio host), and deploy it where
their backend runs, close to agentd: the same host, or the same network. It holds the app's API key, which is
the credential a backend holds today. mansio's operator runs agentd, controld and the edge exactly as now and
needs to know nothing about the runner.

A hosted variant (mansio's operator runs runners for customers) is possible later. It changes who operates the
process, not where code runs relative to agentd, and it brings a deploy pipeline for customer code with it.
The design below does not depend on it.

### How a request flows

```
caller (browser, phone, distant service)
   │  1 wide-area round trip:  POST /fn/mutation/makeMove   {args}   + the user's session
   ▼
runner  (the app owner's process, next to agentd)
   │  authenticates the user, picks the tenant, runs the handler in the SDK's recording executor
   │     reads  → the runner's document cache, else POST /v1/ops (query) over loopback or the local network
   │     writes → buffered in the run
   │  on return: POST /v1/ops (mutation): preconditions + writes, one request id
   ▼
agentd  (unchanged)
      ticket check, admission, the database's actor: validates and applies the operation list
```

Where things execute, hop by hop:

| Hop             | Handler code           | Reads execute                                  | Writes execute                    |
| --------------- | ---------------------- | ---------------------------------------------- | --------------------------------- |
| caller → runner | does not run here      | none                                           | none                              |
| in the runner   | runs here, in the SDK  | cached documents are answered here, unverified | buffered here; nothing is applied |
| runner → agentd | does not run in agentd | in SQLite, on the actor or its read pool       | in SQLite, once, at commit        |

The wide-area cost of a call is one round trip whatever the handler does. The `k + 1` trips of a chain of `k`
dependent reads still exist, but they are loopback trips. The runner's cache is shared by every user it
serves, so it is warm far more often than a single caller's: the common mutation is one loopback trip.

### The trust model

- **Whose code runs in whose process.** The app owner's handlers run in the app owner's process. mansio's host
  code runs there too, as a library. Nothing of the app owner's runs in agentd, controld or the edge.
- **Why the server still never runs tenant code.** agentd receives what it receives today: an operation list.
  It validates shapes against the schema, checks preconditions and applies writes. It cannot tell a runner
  from any other SDK caller, and it does not need to. ADR 0009's property ("any backend that passes the
  vectors serves any app") is untouched.
- **What the end user is trusted with.** Nothing. The end user holds a session with the runner, not a ticket.
  They can call the handlers the runner exposes, with arguments the handler validates. They cannot send
  operations.
- **Where rules are enforced.** In handler code, in the runner, which is server-side from the end user's point
  of view. `makeMove` already refuses a move by the wrong player; today that check is only as trustworthy as
  the caller's process. In the runner it is enforced.
- **What a compromised runner can do.** Whatever its API key can: read and write that app's databases with any
  schema-valid operation. That is the blast radius of an application backend today. It cannot reach other apps
  or tenants it has no key for, and it cannot affect agentd beyond the load its requests cause, which
  admission already bounds.

### What mansio would have to ship

1. **The host** (`sdk/serve.ts`, run as `mansio serve` or imported): a router from
   `/fn/(query|mutation)/:name` to `client.query` / `client.mutation`; argument decoding (`invoke` already
   validates against the declared validators); the error mapping (`localFailure` and `RemoteError` already
   carry status, tag and fields); limits (body size, calls in flight, a deadline per call that the SDK's
   `timeoutMs` already honours); health and drain; `X-Trace-Id` passed through so one trace covers caller,
   runner and agentd.
2. **Identity in the handler.** An `authenticate(request) → identity` hook on the host, and `ctx.auth` beside
   `ctx.db`. Today a handler's context is `{ db }` (`sdk/functions.ts`), so this is an SDK change, additive.
   The identity decides the tenant; the runner keeps one SDK client (and cache) per tenant it is serving.
3. **A caller-side client.** `createRemoteClient<typeof functions>(url)`: the same typed `query` / `mutation`
   surface, importing the functions object as a type only, sending one HTTP request per call.
4. **The caller-to-runner protocol**, written down like `/v1/ops`: the routes, the error body (the same
   `{error: {tag, message, fields}}`), and the retry contract.
5. **A deployment story.** A container image and a `deno task`; a runner in `make platform-up` with the smoke
   going through it; guidance on placement (one runner pool per region where agents run; in multi-node, the
   SDK's locator route already finds the owning agent, through the edge); documentation that finally states
   where callers are meant to run.

Open design point, to settle before building: **exactly-once for the caller.** agentd deduplicates a commit by
request id, but the runner draws that id. If a caller's reply is lost and it calls again, the runner runs the
handler again and commits again. The caller can send an idempotency key and the runner can derive the commit's
request id from it, so the second commit is answered as a duplicate. But a duplicate answers the stored
operation results, not what the handler returned, so the runner must either recompute the return value from a
read or store it. This needs a decision; it does not need a server change.

### What it solves, and what it does not

Solves:

- **Distant callers.** One wide-area round trip per call instead of one per dependent read plus the commit.
- **Callers that cannot hold a write ticket.** Browsers, phones, third parties. This is the gap ADR 0009 left
  open ("server-enforced rules for untrusted callers are out of scope").
- **One deployed version of the logic.** Today two callers with different versions of `makeMove` both commit.
  Behind a runner there is one version: the deployed one.

Does not solve:

- **Conflicts.** The runner is still an optimistic client. Under contention on one document it re-runs. After
  change 3 a re-run costs one loopback trip, so this is a small cost, but it is not zero.
- **Long chains of dependent reads at very high rates.** Each link is a loopback request through agentd's
  admission and actor. Change 6 is the answer to that workload, if it ever appears.
- **Availability.** It is one more process that must be up and reachable.

### Worked example: `makeMove` through a runner

A browser in Sydney, a runner and agentd in Frankfurt. Assume 280 ms for the wide-area round trip and under 1
ms for loopback (illustrative figures, not measured).

1. The browser sends `POST /fn/mutation/makeMove {gameId, playerId, cell, expectedVersion: 7}` with its
   session cookie. One wide-area trip starts.
2. The runner authenticates the session, finds the user's tenant, and takes that tenant's SDK client.
3. The handler runs. `db.get("games", gameId)` is answered from the runner's cache: another player's move went
   through this runner a moment ago and left the game there at version 7. No request.
4. The handler checks `expectedVersion`, checks it is this player's turn (`ctx.auth` says who is asking),
   computes the board, and buffers a patch and an insert.
5. The runner sends one mutation to agentd: `assert games/gameId version 7`, the patch, the insert. One
   loopback trip. agentd applies it and answers sequence 42.
6. The runner caches the game at version 8 and answers the browser. The wide-area trip ends.

Total: 1 wide-area trip and 1 loopback trip, about 281 ms. The same call made by the SDK directly from Sydney
(if the browser could be trusted with a ticket, which it cannot) is 280 ms warm and 560 ms cold, and each
further dependent read adds 280 ms.

If the cache was stale (the other move went through a different runner): step 5's assert fails, agentd answers
the game at version 8, the handler runs again on it, and its own `expectedVersion` check refuses the move.
Cost: the same 1 wide-area trip and 1 loopback trip, and the browser gets the app's conflict.

## Change 6: opt-in execution inside agentd

### How it would work

A handler is marked to run on the server. The app's bundle (handlers, the SDK's executor, the schema) is
registered with controld like a schema is today, under a hash. A call names the handler, its arguments and the
bundle hash. agentd loads the bundle into a sandboxed guest and, for a mutation, runs the handler **inside the
database actor's write savepoint**, in the group-commit batch, where a `/v1/ops` mutation runs now.

The design rule from the survey is the one that keeps this from becoming a second system: the guest hosts the
**same recording executor**, with `Reads` bound to the actor's open transaction instead of the network. The
handler's reads are local SQLite reads. Its writes are buffered. On return the guest hands back the same
operation list the SDK would have sent, and the existing Go executor (`docexec.Run`) applies it. The log
entry, the request-id record and the vectors are unchanged. Because the actor is the database's only writer,
nothing can change between the handler's reads and its writes: the derived preconditions always hold, and
there are no conflicts and no re-runs for these handlers.

### How it differs from change 5

| Question                                         | Change 5: runner                                               | Change 6: inside agentd                                                                                   |
| ------------------------------------------------ | -------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| The code's host process                          | the app owner's runner                                         | agentd, on the database actor's goroutine                                                                 |
| Trust boundary                                   | the network between runner and agentd; agentd sees only IR     | the sandbox wall inside agentd's address space                                                            |
| Who is responsible for sandboxing                | nobody: the app owner runs their own code in their own process | mansio: it runs every tenant's code and must contain it                                                   |
| Resource limits                                  | the app owner's problem; agentd's admission bounds the effect  | mansio must enforce CPU (fuel or a deadline), memory and stack per call, or one handler stalls a database |
| Code versioning and deploys                      | the app owner's deploy; mansio stores no code                  | mansio stores bundles, activates versions per app, rolls back, and decides who may deploy                 |
| What a slow handler does                         | slows its own caller                                           | holds the batch's write lock: every write to that database, and its batch-mates, wait behind it           |
| What a crashing or looping handler does          | crashes or pins the runner                                     | must be interrupted by agentd; if the interrupt fails, the actor is stuck                                 |
| What a sandbox escape reaches                    | not applicable                                                 | agentd: every tenant's database files on that VM, the ticket keys, the object-store credentials           |
| "Any backend passing the vectors serves any app" | holds                                                          | holds only for apps that do not use it; a backend would also need the guest runtime                       |
| Deterministic simulation                         | unaffected                                                     | the guest runtime must be deterministic, or the simulation suite loses its footing                        |

The actor's contract for a closure today reads: it "does no slow I/O: a write holds the database's write lock
for its whole batch", and it "is deterministic given the database state and `tx.Now()`"
(`server/internal/actor/fn.go`). Go code that mansio wrote can promise that. Tenant code cannot promise it, so
agentd would have to enforce it from outside, on every call.

What change 6 buys in exchange: one round trip for any number of dependent reads, no conflict loop at all,
server-enforced rules without a runner, and a single live version of the logic. The survey's verdict stands:
do not build it until a named workload needs it, namely long chains of dependent reads at a high rate, or
contention on one document that cheap re-runs cannot absorb.

## Change 7: WebAssembly or a native isolate

Change 7 is not a placement. Changes 5 and 6 answer "where does the handler run?". Change 7 answers "if the
handler runs inside agentd, what stands between it and agentd?". If change 6 is not adopted, change 7 does not
exist. The Go plan left the same choice to a spike: goja, QuickJS on wazero, or V8 through cgo.

How each would be embedded:

- **A WebAssembly interpreter (QuickJS compiled to WebAssembly, run by a pure-Go runtime).** agentd links a Go
  WebAssembly runtime. The guest module is a JavaScript interpreter; the app's bundle is data handed to it.
  The module can do nothing but compute and call the host functions agentd exports, which would be one:
  "answer these reads from the open transaction". Its memory is a byte array agentd allocated, with a cap.
  agentd stops a call that runs too long.
- **A native isolate (V8 through cgo).** agentd links V8, a large C++ library, and creates an isolate per app
  or per call. The bundle is compiled by V8's JIT. Host functions are registered as callbacks. Limits are V8's
  heap limit and a watchdog thread that terminates execution.
- **goja** (a JavaScript interpreter in Go) is the third candidate: the simplest to embed, with interruption
  but no memory cap of its own.

Why the survey recommended the WebAssembly interpreter for mansio:

- **No cgo.** The server is pure Go today. cgo changes the build, the cross-compilation the e2e script relies
  on, the crash behaviour (a fault in C++ takes the process) and the scheduler's view of blocked threads.
- **Isolation by construction.** A WebAssembly guest has no ambient I/O, no pointers into the host and a
  bounded memory. With V8 the isolation is V8's own, a very large trusted computing base whose JIT is a
  recurring source of escapes.
- **Determinism.** An interpreter gives the same result for the same input and can be stopped at the same
  point. The simulation suite and the "a closure is deterministic" contract depend on that. A JIT with
  background compilation and garbage-collection threads does not offer it.
- **Limits.** Memory is capped by the runtime. For CPU, a spike must confirm the mechanism: whether the chosen
  runtime can count instructions (fuel) or only interrupt on a deadline. A deadline is enough to protect the
  actor; fuel is what makes the limit deterministic.

What it costs: speed. A JavaScript interpreter running inside a WebAssembly interpreter is slow next to V8,
and the Effect runtime the handlers are written in has never been measured there. The handler's work is mostly
waiting for local SQLite, so it may be acceptable. That is the first thing the spike must measure, and change
6 should not be approved on an estimate.

## Side by side

Change 7 has no column of its own in most rows because it is a choice inside change 6; where the choice
matters it is shown as 6 with each sandbox.

|                        | 5: colocated runner                                                   | 6: inside agentd                                                                               | 7: the sandbox for 6                                                                                     |
| ---------------------- | --------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| Where code runs        | in a runner process beside agentd                                     | in agentd, inside the database's write transaction                                             | same as 6: in a WebAssembly guest, or in a native isolate                                                |
| Whose process          | the app owner's                                                       | mansio's                                                                                       | mansio's                                                                                                 |
| Trust boundary         | the network; agentd accepts only IR                                   | the sandbox                                                                                    | WebAssembly: a bounded memory and one host function. Isolate: V8's own isolation                         |
| What mansio must build | host, `ctx.auth`, remote client, caller protocol, deployment and docs | bundle registry and deploys, call route, guest host, limits, observability, simulation support | WebAssembly: embed a pure-Go runtime and a guest interpreter build. Isolate: cgo, V8 builds per platform |
| Blast radius           | one app's data, through its own key                                   | a stalled handler: one database's writes. An escape: every tenant on the VM                    | WebAssembly: smaller trusted base, memory-safe host. Isolate: larger base, a JIT, C++ faults kill agentd |
| Read latency           | cache hit: none. Otherwise one loopback request per batch of reads    | in-process, in the transaction                                                                 | same as 6, times the interpreter's overhead (unmeasured); V8 is much faster                              |
| Write latency          | one loopback request, plus one per conflict                           | none beyond the commit; no conflicts                                                           | same as 6                                                                                                |
| Caller's cost          | one round trip per call                                               | one round trip per call                                                                        | same as 6                                                                                                |
| Server changes         | none                                                                  | large, in the most sensitive code                                                              | part of 6                                                                                                |

## Local logic later: is it still feasible?

"Local logic" here means the survey's placement D: the caller runs a handler locally, against local data, to
get an answer at once, and the server remains the authority that accepts or corrects it. Nick asked that
changes 1 to 4 not foreclose it. This section judges that for the code as built, and for changes 5 and 6.

### After changes 1 to 4, as implemented

What already lines up:

- **The executor is reusable as it is.** `Recording` takes `Reads`, an id source, a clock, a `seed` and a
  `cache` (`RecordingOptions`). None of them is a transport. A speculative run is a `Recording` whose reads
  are answered locally.
- **A version-checked local read is already the rule.** Change 4 is local execution for point reads: the
  handler computes on cached documents, and the commit proves them. `ReadCache` is an interface, so a synced
  local store can stand behind it. `pending` and `verify` are the rule a speculative answer needs ("not
  believed until asserted").
- **The read set is plain data in the protocol's own terms.** Guards are IR asserts (`Guard`): a document at a
  version, a table unchanged since a sequence. A local run emits the same ones.
- **The server's verdict is enough to rebase.** A conflict carries the sequence it was decided at and the
  current state behind every failed precondition (spec §8.1), and `Recording.rebase` turns it into a `ReadSet`
  the next run starts from. That is the reconcile step of placement D, already running on every conflict.
- **Snapshot identity is visible.** Every answer carries `seq`; `Recording.snapshot` and `ReadSet.seq` name
  the state a run saw. Each table's last change is stored on the server (`_mansio_changes`), which is the
  invalidation primitive a local store needs.
- **Handlers are already replayable.** They may run more than once, have no effects beyond `db`, and draw ids
  on the client.

What is still missing:

1. **Local data beyond point reads.** The cache answers `get`. A local `find` needs every row of the table as
   of a known sequence, and a select evaluator over them (the overlay's `matches` and `compareRows` are most
   of one). That means a local store and a way to fill it: the plan's read replicas or an op log
   (`docs/plans/completed/intermediate-representation-protocol.md` §7, steps 1 and 2).
2. **A change feed.** Today the client learns of other writers' changes only by being refused. Local logic
   wants "what changed since sequence N" pushed or pulled. The sequence exists; the feed does not.
3. **A pending-write queue with rebase.** Showing a speculative result before the commit returns, queueing
   further calls on top of it, and re-running them in order when the server disagrees. The pieces (re-run from
   a `ReadSet`) exist; the queue and its API do not.
4. **Exact knowledge of what a commit stored.** A mutation's answer does not say the clock reading the server
   stamped on inserts, so the client cannot know an inserted document exactly. Change 4 therefore does not
   cache inserts, and a local store could not hold them without reading them back.

Cost: item 3 for handlers that read by id is small, on top of change 4. Items 1 and 2 are the large part, the
same work as the survey's change 8 and the plan's local-first steps. Item 4 is a few lines.

### Under change 5's runner

The runner changes who "the caller" is. For agentd, the runner is the caller, and everything above holds for
it unchanged. The interesting case is local logic in the end user's client (a browser), which sits behind the
runner.

- **What lines up.** That client cannot send operations, so the authority must re-run the handler: it sends
  the call, the runner executes it. That is exactly placement D's shape, and the runner is the piece that
  makes it possible for untrusted clients. The same `functions` object and the same executor run on both
  sides.
- **What is missing.** The local data and feed of the previous list, with one addition: a browser has no read
  ticket either, so its data must come through the runner, filtered by the runner's rules. Reads through the
  runner need authorization the way calls do. And the caller protocol must return enough for the client to
  reconcile: the documents the call touched, with versions.
- **Cost.** Medium to large, and all of it on top of the runner. Nothing in the runner's design blocks it, if
  the caller protocol is written with a result envelope that can later carry touched documents.

### Under change 6

- **What lines up.** Both sides run the same bundle, pinned by its hash, so a local prediction can differ from
  the server's result only because the data differed, never because the code did. The authority's run cannot
  conflict. This is the cleanest form of placement D.
- **What is missing.** The same local store and feed. Change 6 adds nothing to them and removes nothing.
- **Cost.** Change 6's own cost dominates. Local logic does not need change 6, and change 6 does not need
  local logic.

### Verdict

**Feasible as an evolution.** Nothing in changes 1 to 4 as built, in the runner, or in in-agentd execution
forecloses local logic, and changes 1 to 4 already contain its core: a transport-free executor,
version-checked local reads, read sets as data, and a conflict answer that a local run can rebase on.
Speculative execution of handlers that read by id is a small step from here. The full version (local `find`,
offline work) needs two named pieces of preparatory work that are large and are not needed yet: a local store,
and a change feed to keep it current.

The smallest preparation that could ride along now: **have a mutation's answer carry the request's clock
reading** (the server already takes exactly one per request, spec §6.3). With it the client knows every
document its commit inserted, exactly; inserts become cacheable, which also makes the first `makeMove` after
`createGame` one round trip, and a local store can apply its own commits without reading them back. It was not
built in this run; it is recorded in `agent-work/STATE.md` as a candidate.

## What approving change 5 would set in motion

Proposed build order, each step shippable on its own:

1. **Write the caller protocol** (routes, error body, retry contract, the result envelope) and settle the
   exactly-once question above. A document and vectors, no code.
2. **`ctx.auth`**: the identity hook and the handler context, in the SDK, with the sample app's `makeMove`
   using it to check whose turn it is.
3. **The host**: `sdk/serve.ts` and `mansio serve`, with limits, health, drain and trace pass-through; one SDK
   client and cache per tenant.
4. **The remote client**: `createRemoteClient`, typed from the functions object, and the SDK's existing unit
   tests run through it against a runner in process.
5. **Platform wiring**: a runner in `make platform-up`, the smoke calling through it, an e2e scenario with an
   untrusted caller that is refused a move it may not make.
6. **Documentation**: where callers are meant to run, the two supported topologies (SDK in a trusted backend;
   remote client through a runner), and placement in multi-node.
7. **Measure**: the wide-area and loopback trip counts per call, and the runner's cache hit rate under the k6
   scenario, so the next decision (whether anything needs change 6) is made on numbers.
