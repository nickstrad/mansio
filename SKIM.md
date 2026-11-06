# Skim the Effect document database

These excerpts are copied from the source. Omitted sections are described in prose, not rewritten as alternate
implementations. The server is the Go code in `server/`; this tour covers the TypeScript side: the app, the
SDK, the pure compiler and the offline event-log CLI. See [README.md](README.md) for the layout and
verification.

## Application schema and handler

Source: [app/schema.ts](app/schema.ts).

<!-- deno-fmt-ignore -->
```ts
import { defineSchema, defineTable, v } from "../sdk/schema.ts";
export const mark = v.enum("X", "O");

export const schema = defineSchema({
  games: defineTable({
    xPlayer: v.string(),
    oPlayer: v.string(),
    board: v.array(v.nullable(mark), 9),
    turn: mark,
    status: v.enum("playing", "won", "draw"),
    winner: v.nullable(mark),
  }).index("by_status", ["status"]),

  moves: defineTable({
    gameId: v.string(),
    playerId: v.string(),
    mark,
    cell: v.integer(),
    moveNumber: v.integer(),
  }).index("by_game", ["gameId", "moveNumber"]),
});
```

The schema remains TypeScript data. Mutations compose document operations as effects; the following member
belongs to the exported functions object, not a separate Promise-based handler.

Source: [app/functions.ts](app/functions.ts).

<!-- deno-fmt-ignore -->
```ts
  createGame: mutation({
    args: { xPlayer: v.string(), oPlayer: v.string() },
    handler: ({ db }, { xPlayer, oPlayer }) =>
      Effect.gen(function* () {
        yield* validatePlayers(xPlayer, oPlayer);
        const gameId = yield* db.insert("games", {
          xPlayer,
          oPlayer,
          board: Array(9).fill(null),
          turn: "X",
          status: "playing",
          winner: null,
        });
        return { gameId };
      }),
  }),
```

## Document operations describe effects

Source: [sdk/db.ts](sdk/db.ts).

<!-- deno-fmt-ignore -->
```ts
export type Operation =
  | SelectPlan
  | { kind: "insert"; table: string; value: unknown }
  | { kind: "patch"; table: string; id: string; value: unknown }
  | { kind: "delete"; table: string; id: string };
export type Execute = (operation: Operation) => Effect.Effect<unknown, DbError>;
```

## A terminal read returns a typed Effect

Source: [sdk/db.ts](sdk/db.ts).

<!-- deno-fmt-ignore -->
```ts
collect(): Effect.Effect<T[], DbError> {
  return Effect.map(this.execute(this.plan), (rows) => rows as T[]);
}
first(): Effect.Effect<T | null, DbError> {
  return Effect.map(this.limit(1).collect(), (rows) => rows[0] ?? null);
}
```

## Pure compilation reports typed errors

Source: [core/compile/select.ts](core/compile/select.ts).

<!-- deno-fmt-ignore -->
```ts
export function tableFor(schema: Schema, name: string): Either.Either<Table, CompileError> {
  if (!Object.hasOwn(schema, name)) {
    return Either.left(new CompileError({ reason: "unknown-collection", table: name }));
  }
  return Either.right(schema[name]);
}
```

`compileSelect` in the same file normalizes the plan and returns `Either<SQL, CompileError | SchemaError>`.
The compiler returns SQL text with bound values. It is the oracle for the Go server's port
(`server/internal/doccompile`, ADR 0010): both are checked against the shared vectors in `ir/vectors/`.
`GENERATED_SQL.txt` shows actual output for both layouts and dialects.

## A mutation is recorded, then committed once

Source: [sdk/recording.ts](sdk/recording.ts).

<!-- deno-fmt-ignore -->
```ts
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
```

A `Recording` is the `db` a handler sees in the caller's process. Reads go to the server at once and are
cached per run; writes are buffered and later reads in the same run see them. Every document a `get` observed
becomes an `assert` at the front of the mutation, so a conflict on an op index below `asserts` is always one
the client derived.

Source: [sdk/client.ts](sdk/client.ts).

<!-- deno-fmt-ignore -->
```ts
      for (let run = 0;; run++) {
        const recording = new Recording<Schema>(reads(call), newId, now, true);
        const result = yield* roundTrip(yield* runHandler(definition, recording, args));
        const { ops, asserts } = recording.mutation();
        if (ops.length === 0) return result; // read nothing, wrote nothing: nothing to commit
        // (an op-limit check, failing locally with RuleError, is omitted here)
        // A re-run is a new request: its op list differs (fresh reads, fresh ids), and the last one failed.
        const committed = yield* Effect.either(
          Effect.flatMap(request("mutation", ops, newRequestId()), (req) => send(req, call)),
        );
        if (committed._tag === "Right") return result;
        const error = committed.left;
        if (!derivedConflict(error, ops, asserts) || run >= handlerRetries) return yield* error;
        // Someone else wrote what this run read: re-run the handler on fresh reads, after a jittered pause.
        const wait = Math.floor(random() * Math.min(250, 10 * 2 ** run));
        if (now() + wait >= call.deadline) return yield* error;
        yield* sleep(wait);
      }
```

The handler runs again on a derived conflict, which is why handlers must have no effects beyond `db`. `send`
(`sdk/transport.ts`) owns transport retries, reroutes and the retry budget; the same `requestId` on a
transport retry lets the server answer an applied mutation as a duplicate.

## The event-log CLI is a layer chain

Source: [eventlog/root.ts](eventlog/root.ts).

<!-- deno-fmt-ignore -->
```ts
/** `Config` from the environment and the CLI's own event sink, in the order of the module comment. */
export const CliRootLive = LoggingReaderLive.pipe(Layer.provideMerge(ConfigLive));
```

Each `scripts/logs.ts` command is its own Effect program. `ingest` and `sweep` run on `CliRootLive`
(configuration from the environment, then the CLI's own event sink) and build the event-log handle in the
program's scope; `query` and `list` need only `Config` and the read-only `AnalysisLive`. `ConfigLive`
(`eventlog/config.ts`) is the only module that reads the environment; it fails with a typed `ConfigError` at
layer construction. Service tags are declared in `eventlog/services.ts`.

## Native handles register finalizers immediately

Source: [eventlog/ingest.ts](eventlog/ingest.ts).

<!-- deno-fmt-ignore -->
```ts
    const instance = yield* Effect.acquireRelease(
      attempt("inventory", () => driver.open(dbPath)),
      (handle) => close("instance", handle),
    );
```

The ingest opens the DuckDB instance under `acquireRelease`, then its connection, and registers the
connection's finalizer after the instance's, so LIFO release closes the connection first.
`eventlog/duckdb.ts`'s `drainDuckDB` forces pending N-API finalizers to run before the handles close.

## Run

```sh
deno task explain > GENERATED_SQL.txt
work-scripts/bash/deno-linux.sh 'deno task ci'
deno task go:ci
work-scripts/bash/e2e-linux.sh
```

`deno task ci` runs on Linux because the DuckDB event-log suites hang on macOS. See the README's Verification
section.
