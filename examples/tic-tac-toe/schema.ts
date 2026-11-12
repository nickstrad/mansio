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
