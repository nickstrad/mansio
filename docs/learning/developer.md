# Learning path: developer

For someone who wants to understand the platform well enough to change it. Work through this top to bottom.
Each step is something to run, read or try, followed by why it matters. Phases go from "see it work" to
"understand each part" to "understand how it fails" to "make a change". Do the [customer](customer.md) path
first if you have never used the SDK. Paths are from the repo root. Setup for every command:
`export PATH="$HOME/.deno/bin:$PATH"`.

## Phase 1: See the whole thing once

1. Read the first screen of `README.md` (down to "Repository layout").

   It gives the one-sentence shape: handlers run in the caller's process inside the TypeScript SDK, and the Go
   server only executes a small protocol (the intermediate representation, or IR) against SQLite. Everything
   else in this guide is detail on that sentence.

2. Open <https://nickstrad.github.io/mansio/> and click through `context` → `containers`.

   These diagrams are the map for the rest of the guide: caller + SDK, controld, the edge, agentd, SQLite
   files, Postgres placement, S3. Keep the tab open and find each thing you study on it.

3. On Linux, run `make platform-up`, read what it prints, then `make platform-down`. (The script needs GNU
   tools; on macOS it stops with `realpath: illegal option -- m`, so use a Linux machine or VM.)

   This builds and starts the real multi-node shape (controld, two agentd, the edge, Postgres, SeaweedFS) and
   proves it with one SDK write and read through the edge. Seeing it start is the fastest proof that the
   pieces on the diagram are real processes.

4. Read `scripts/platform/smoke.ts` and `scripts/platform/up.sh`.

   The smoke script is the shortest complete client program in the repo, and `up.sh` shows exactly which
   config each process gets. Together they answer "what does it take to run this?".

## Phase 2: The app author's view (TypeScript)

5. Read `examples/tic-tac-toe/schema.ts`, `examples/tic-tac-toe/functions.ts` and
   `examples/tic-tac-toe/rules.ts`.

   This tic-tac-toe app is what a user of the platform writes: typed tables and handlers, no SQL. Every later
   layer exists to make these three files work.

6. Read [`typescript-tour.md`](typescript-tour.md) (in this folder) top to bottom.

   It is a guided tour of the Effect code with excerpts, written for exactly this purpose. It introduces the
   "a handler run is recorded, then committed once" idea that the SDK is built around.

7. Follow `docs/guide.md` from "Installation" through "SDK walkthrough": start a single agentd by hand and
   call it.

   Writing the config and tokens yourself removes the magic of `platform-up`. Single-node static mode is the
   simplest form of the system, so learn it before the multi-node one.

8. Read "App-facing client" and "Query language and semantics" in `README.md`.

   This is the contract: one snapshot per run, buffered writes, preconditions derived from reads, and the
   handler re-running on conflict. The rule "handlers must have no side effects beyond `db`" comes from here.

9. Read `sdk/db.ts`, then `sdk/recording.ts`, then `sdk/client.ts`.

   `db.ts` is the API handlers call, `recording.ts` turns one handler run into reads plus one commit, and
   `client.ts` is the loop that re-runs on conflict. This is the most important TypeScript in the repo.

10. Read `sdk/cache.ts` and `docs/notes/intermediate-representation/2026-11-10-read-path.md`.

    The cache is why a warm mutation is one round trip, and the note lists the traps (for example, a cached
    read is never trusted, only asserted). Read the note's "Traps" section twice.

11. Run `deno test -A tests/unit/sdk_read_path_test.ts` and read the test names.

    Each test counts requests, so the file doubles as an executable description of the read path. Change one
    expected count, watch it fail, and you will know what the SDK really sends.

## Phase 3: The wire (the IR protocol)

12. Read `intermediate-representation/spec/v1.md` sections 1 to 6.

    The IR is the only thing that crosses between TypeScript and Go, so it is the true interface of the
    platform. Sections 5 (operations) and 6 (execution order, atomicity) are the core.

13. Read sections 7 and 8 of the spec (request ids, duplicates, errors, what a `ConflictError` carries).

    These explain how a retry is safe and how the SDK knows what changed. Most "what happens if…" questions
    are answered here.

14. Open `intermediate-representation/vectors/README.md` and look at a few cases in
    `intermediate-representation/vectors/execute.json`.

    The vectors are shared test cases both the TypeScript and Go sides must pass, which is what keeps two
    implementations agreeing. Reading three or four vectors makes the spec concrete.

15. Run `deno task explain` and compare with `core/compile/GENERATED_SQL.txt`; skim `core/compile/select.ts`
    and `core/compile/write.ts`.

    This shows the SQL a document operation becomes, in both storage layouts. The TypeScript compiler is the
    oracle the Go compiler is tested against.

## Phase 4: The data plane (Go, one agentd)

16. Read `server/cmd/agentd/main.go` and the `Config` type in `server/cmd/agentd/config.go`.

    `main.go` is the wiring: it shows which packages make up agentd and in what order they start. The config
    comments are the best reference for what each knob does.

17. Read `server/internal/front/ops.go`, then `admission.go` in the same folder.

    This is the path of one `POST /v1/ops` request: verify the caller, decode, admit, dispatch. Admission is
    where overload is refused, so it explains every `shed` 503 you will see.

18. Read `server/internal/actor/actor.go` and `reqid.go`.

    One goroutine per database with a mailbox and group commit is the central design choice of the server. The
    request-id table is what makes a duplicate request replay instead of applying twice.

19. Read `server/internal/docexec/doc.go`, then `exec.go`, `seq.go` and `version.go`.

    This executes IR inside the actor: preconditions, operations, `_version` and the commit sequence. The
    floors in `seq.go` and `version.go` are why versions never repeat after a crash.

20. Skim `server/internal/doccompile/doc.go` and one compile file next to it.

    This is the Go twin of `core/compile/`. You only need to see that it is the same idea; the vectors prove
    they match.

21. Open the `components` view on the diagram site and trace steps 17 to 20 on it, then open `query-flow`.

    You have now read every box on that view. If any box is still unclear, reread its file before moving on.

22. Run `cd server && go test ./internal/actor ./internal/docexec` and read two tests of your choice.

    Tests are where the intended behaviour is stated plainly, which matters in code you did not write by hand.
    Pick tests whose names you cannot yet explain.

## Phase 5: The control plane (Go, many agents)

23. Read `server/docs/control-plane-plan.md` (skim for the decisions, not the task lists).

    It states why there is a control plane at all and what it promises. Read it before the code, since the
    code only makes sense against those promises.

24. Read `server/internal/control/locator/doc.go`, then `server/internal/ticket/` and `sdk/transport.ts`.

    This is "resolve once, then go direct": the locator answers where a database lives and signs a ticket, and
    the SDK caches it. It is why controld is not on the path of every query.

25. Read `server/internal/placement/pg/doc.go` and `server/internal/owner/state.go`.

    Postgres holds who owns each database and at which epoch; the agent holds its local view of the same.
    Epochs and fencing are how two agents never both serve one database.

26. Read `server/internal/owner/move.go`, then open the `database-move` view on the diagram site.

    The four-step move (mark moving, fence, flip, claim; the final write-back happens inside the fence) is the
    hardest protocol in the system. The diagram and the file describe the same steps, so read them side by
    side.

27. Read `server/cmd/edge/main.go` and `server/internal/edge/proxy.go`.

    The edge is small: verify the ticket, route by VM, refuse early when overloaded. Reading it confirms there
    is no database logic in front of agentd.

## Phase 6: Durability and what is paused

28. Read `server/docs/notes/writeback/2026-11-06-backup-paused-open-issues.md`.

    It says plainly what the S3 write-back does and does not guarantee, and why the work is paused. This is
    the most important "known limits" document in the repo.

29. Read "Backups and restore" in `docs/guide.md`, then skim `server/internal/vactor/writeback.go` and
    `server/internal/durable/durable.go`.

    Durable mode stores each database as immutable generations in S3 with a manifest. The key fact to keep:
    the acknowledgement is the local commit, so a crash can lose the last seconds of writes.

30. Read `server/docs/notes/writeback/2026-11-10-version-floor.md`.

    It is a worked example of a real bug (versions repeating after a rollback) and its fix. It also shows how
    the simulator found it, which leads into the next phase.

## Phase 7: How it is tested, and how it fails

31. Read `docs/testing.md`, then run `deno task go:ci` (and `deno task ci` on Linux via
    `scripts/dev/deno-linux.sh 'deno task ci'`).

    This is the gate every change must pass, and the document says what each suite proves. Knowing the gates
    tells you how much to trust a change you did not write.

32. Run `scripts/dev/e2e-linux.sh` and read two files in `tests/e2e/platform/scenarios/`.

    The e2e suite drives the real Go binaries through the SDK, so it is the closest thing to a user's
    experience. The scenarios read like a list of promised behaviours.

33. Read `server/internal/sim/doc.go` and `server/docs/notes/determinism/README.md`, then run
    `deno task sim:swarm 20` (20 seeds, about 15 seconds; with no argument it runs 2500 seeds for hours).

    The deterministic simulator runs a cluster with injected faults and checks invariants such as "no version
    is reused" (`cluster_model.go`). It is the main reason to believe the move and fencing protocols are
    correct.

34. Read `server/docs/notes/backpressure/README.md` and `docs/retry-contract.md`.

    Together they describe what happens under overload: who refuses, with which code, and what each client
    does next. This is the operational behaviour you would see first in production.

35. Read `server/docs/capacity.md`.

    One table per process (agentd, controld, the edge) lists every queue, its limit, its metric and its alert.
    When something is slow or refusing, this is the page that says where to look.

## Phase 8: Observability and history

36. Read "Observability" in `docs/guide.md` and run its block against the agentd from step 7: stop agentd,
    then `deno task logs:ingest`, `deno task logs:list` and
    `deno task logs:query handler-throughput --format json`.

    The server writes a JSONL event log and an offline CLI loads it into DuckDB for named queries. This is how
    you answer "what did the system actually do?" after the fact.

37. Start that agentd again, make a few calls, and run
    `curl -s http://127.0.0.1:7070/metrics | grep mansio_queue`.

    The event log is for after the fact; `/metrics` is the live view. Matching each `queue` label to a row of
    `server/docs/capacity.md` ties the overload reading from steps 34 and 35 to numbers you can watch.

38. Read `docs/adr/README.md`, then ADRs 0009, 0010 and 0013.

    These three record the decisions that produced the current shape: the IR as the wire, the compiler port
    with a TypeScript oracle, and the Go server replacing the Deno platform. They explain leftovers you will
    see in older docs.

39. Skim `docs/notes/index.md`, `docs/notes/gotchas.md` and `server/docs/notes/index.md`.

    These are the running notes of decisions and traps. You do not need to read them all, only to know they
    exist and how they are organised.

40. Read the last two weeks of `agent-work/STATE.md` (newest first) and `agent-work/server/shared_state.md`
    (newest at the bottom): the logs the build agents kept.

    This is the record of what changed most recently and why. It is the best way to catch up after time away.

## Phase 9: Prove you understand it, then change it

41. Add a small feature to the sample app (for example a `resignGame` mutation in
    `examples/tic-tac-toe/functions.ts`), call it from a script against your local agentd, and watch the SQL
    in the event log.

    Using the platform as an app author tests everything from phases 2 to 4 at once. If you can predict the IR
    and the SQL before you look, you understand the request path.

42. Explain out loud, without notes: what happens when two clients call `makeMove` on the same game at the
    same moment?

    A full answer touches the SDK's derived preconditions, the actor's single writer, the `ConflictError` with
    current state, and the handler re-run. If a part is fuzzy, go back to that phase.

43. Explain the same way: what happens to an in-flight request while its database is being moved to another
    agent?

    A full answer covers fencing, the epoch flip, `wrong_owner`, the SDK re-resolving, and the retry contract.
    This is the test for phases 5 and 7.

44. Read `CLAUDE.md`, `server/docs/coding-style.md` and, for the agent workflow, `agent-work/AGENTS.md`.

    These are the working rules: commit style, branches, the gates, and how the Go code is expected to look.
    Most of the code was written by agents following these files, so they explain its habits.

45. Trace one existing feature end to end through git: `git log --oneline 8fa7779^1..8fa7779^2`, then read the
    commit `2868127` ("guard find with a table precondition").

    One commit touches the spec, the vectors, the SDK and the Go executor together. That is the shape every
    protocol change takes, and the order to copy.

46. Make a small real change with that shape: for example add a `resignGame` test to
    `tests/e2e/platform/scenarios/`, or a new named query to the event-log catalog (`eventlog/`), and pass
    `deno task ci:all`.

    A change that passes the gates proves you can extend the platform, not only read it. Start with something
    that needs no protocol change.

47. Record what you learned as a dated note under `docs/notes/` or `server/docs/notes/` (rules:
    `agent-work/docs-knowledge/AGENTS.md`).

    The notes folders are how understanding survives between sessions. Writing a note is also the best check
    that you understood the thing.
