# A tour of the TypeScript side

These excerpts are copied from the source. Omitted sections are described in prose, not rewritten as alternate
implementations. The server is the Go code in `server/`; this tour covers the TypeScript side: the app, the
SDK and the pure compiler. See [README.md](../../README.md) for the layout and verification.

## Application schema and handler

Source: [examples/tic-tac-toe/schema.ts](../../examples/tic-tac-toe/schema.ts).

<!-- deno-fmt-ignore -->
```ts
import { defineSchema, defineTable, v } from "../../sdk/schema.ts";
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

Source: [examples/tic-tac-toe/functions.ts](../../examples/tic-tac-toe/functions.ts).

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

Source: [sdk/db.ts](../../sdk/db.ts).

<!-- deno-fmt-ignore -->
```ts
export type Operation =
  | SelectPlan
  | { kind: "insert"; table: string; value: unknown }
  | { kind: "patch"; table: string; id: string; value: unknown }
  | { kind: "delete"; table: string; id: string };
export type Execute = (operation: Operation, guarded?: boolean) => Effect.Effect<unknown, DbError>;
```

## A terminal read returns a typed Effect

Source: [sdk/db.ts](../../sdk/db.ts).

<!-- deno-fmt-ignore -->
```ts
collect(): Effect.Effect<T[], DbError> {
  return Effect.map(this.execute(this.plan, this.guarded), (rows) => rows as T[]);
}
first(): Effect.Effect<T | null, DbError> {
  return Effect.map(this.limit(1).collect(), (rows) => rows[0] ?? null);
}
```

## Pure compilation reports typed errors

Source: [core/compile/select.ts](../../core/compile/select.ts).

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
`core/compile/GENERATED_SQL.txt` shows actual output for both layouts and dialects.

## A handler run is recorded, then committed once

Source: [sdk/recording.ts](../../sdk/recording.ts).

<!-- deno-fmt-ignore -->
```ts
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

  /** The mutation's operations: the derived asserts, then the buffered writes in order. */
  mutation(): { ops: Op[]; asserts: number } {
    const asserts = this.preconditions();
    return { ops: [...asserts, ...this.ops], asserts: asserts.length };
  }
```

A `Recording` is the `db` a handler sees in the caller's process. Reads asked for together are one request,
and every read request after the first carries the preconditions of what the run has read so far, so the reads
of one run hold in one snapshot. Writes are buffered and later reads in the same run see them. Every document
a `get` observed becomes an `assert` on its version, and every table a `find` read becomes an `assert` that it
has not changed since; they go at the front of the mutation, so a conflict on an op index below `asserts` is
always one the client derived. A mutation's `get` may be answered from the client's cache (`sdk/cache.ts`);
the cached version is asserted the same way.

Source: [sdk/client.ts](../../sdk/client.ts) (inside the loop that runs a handler until a run stands).

<!-- deno-fmt-ignore -->
```ts
        // The run's outcome before any commit: the handler's answer as it will cross the wire, or why
        // there is none (its failure, a run the recording abandoned, a commit too large to send).
        let ran = yield* Effect.either(
          runHandler(definition, recording, args).pipe(
            Effect.flatMap(roundTrip),
            Effect.filterOrFail(() => !tooManyOps(recording), () => opLimit(name, recording)),
          ),
        );
        const { ops, asserts } = recording.mutation();
        const commits = ran._tag === "Right" && ops.length > asserts;
        // A run that ends without a commit answers from its reads alone: any that came from the cache are
        // asserted first, and a stale one re-runs the handler.
        if (!commits && recording.pending && !(ran._tag === "Left" && ran.left instanceof Restart)) {
          const verified = yield* Effect.either(settle(recording.verify()));
          if (verified._tag === "Left") {
            ran = verified;
          }
        }
        let error: ClientError;
        if (ran._tag === "Left") {
          if (!(ran.left instanceof Restart)) {
            return yield* ran.left;
          }
          error = ran.left.error;
          seed = ran.left.seed;
        } else {
          if (!commits) {
            return ran.right; // wrote nothing: its reads were one snapshot already
          }
          // A re-run is a new request: its op list differs (fresh reads, fresh ids), and the last one failed.
          const committed = yield* Effect.either(
            Effect.flatMap(request("mutation", ops, newRequestId()), (req) => send(req, call)),
          );
          if (committed._tag === "Right") {
            recording.committed(committed.right.result.slice(asserts));
            return ran.right;
          }
          error = committed.left;
          recording.forget();
          if (!derivedConflict(error, ops, asserts)) return yield* error;
          seed = recording.rebase(error as RemoteError, ops.slice(0, asserts) as Guard[]);
        }
        // (a jittered pause, bounded by handlerRetries and the call's deadline, is omitted here)
```

The handler runs again on a derived conflict, which is why handlers must have no effects beyond `db`. The
conflict carries the current state of what the run read, and the next run starts from it (`seed`) instead of
reading again. `send` (`sdk/transport.ts`) owns transport retries, reroutes and the retry budget; the same
`requestId` on a transport retry lets the server answer an applied mutation as a duplicate.

## Run

```sh
deno task explain > core/compile/GENERATED_SQL.txt
scripts/dev/deno-linux.sh 'deno task ci'
deno task go:ci
scripts/dev/e2e-linux.sh
```

`deno task ci` runs in a Linux container through `deno-linux.sh`: natively on macOS one property test fails on
a SQLite rounding difference. See the README's Verification section.
