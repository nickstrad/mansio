// Application code: no SQL, database drivers, file paths, or storage-mode branches.
// The rules live in ./rules.ts; these handlers are the plumbing that reads and writes documents.
import { Effect } from "effect";
import { ConflictError, NotFoundError } from "../core/errors.ts";
import { defineFunctions } from "../sdk/functions.ts";
import { v } from "../sdk/schema.ts";
import { applyMove, validatePlayers } from "./rules.ts";
import { schema } from "./schema.ts";
const { query, mutation } = defineFunctions(schema);

export const functions = {
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

  makeMove: mutation({
    args: { gameId: v.string(), playerId: v.string(), cell: v.integer(), expectedVersion: v.integer() },
    handler: ({ db }, { gameId, playerId, cell, expectedVersion }) =>
      Effect.gen(function* () {
        // Answered from the client's cache when it holds the game, else by one query. Either way the
        // version read here is asserted by the commit.
        const game = yield* db.get("games", gameId);
        if (!game) return yield* new NotFoundError({ table: "games", id: gameId });
        if (game._version !== expectedVersion) {
          return yield* new ConflictError({
            expected: expectedVersion,
            actual: game._version,
            table: "games",
            id: gameId,
          });
        }
        const next = yield* applyMove(game, playerId, cell);
        yield* db.patch("games", gameId, next.game);
        yield* db.insert("moves", next.move);
        // Answered locally: the game as this run's patch leaves it. The patch and the insert commit together.
        return yield* db.get("games", gameId);
      }),
  }),

  getGame: query({
    args: { gameId: v.string() },
    handler: ({ db }, { gameId }) => db.get("games", gameId),
  }),

  listGames: query({
    args: { status: v.enum("playing", "won", "draw") },
    handler: ({ db }, { status }) =>
      db.find("games", { status })
        .sort("_creationTime", "desc").limit(20).collect(),
  }),

  moveHistory: query({
    args: { gameId: v.string() },
    handler: ({ db }, { gameId }) =>
      db.find("moves", { gameId })
        .sort("moveNumber").limit(9).collect(),
  }),

  winsByMark: query({
    args: {},
    // Compiles to a filtered GROUP BY + COUNT in DuckDB, not JS aggregation.
    handler: ({ db }) => db.find("games", { status: "won" }).countBy("winner"),
  }),

  deleteGame: mutation({
    args: { gameId: v.string() },
    handler: ({ db }, { gameId }) =>
      Effect.gen(function* () {
        const game = yield* db.get("games", gameId);
        if (!game) return yield* new NotFoundError({ table: "games", id: gameId });
        const moves = yield* db.find("moves", { gameId }).limit(9).collect();
        for (const move of moves) yield* db.delete("moves", move._id);
        yield* db.delete("games", gameId);
        return { deleted: true };
      }),
  }),
};
