/**
 * The tic-tac-toe rules, as pure functions (plan item 10, work 3).
 *
 * This module is PURE in the same sense as `core/`: it imports `effect`, the pure `RuleError`
 * class from `core/errors.ts`, and types only — no `Effect`, no `sdk/` runtime, no server code,
 * no `Deno.*`, no I/O. Every rejection is a `RuleError` in the left channel of an `Either`, so
 * the whole game can be exercised without a database (`tests/unit/rules_test.ts`, and the
 * property suite in item 12).
 *
 * Note on the type parameter order: the plan writes `applyMove`'s signature error-first
 * (`Either<RuleError, …>`), but Effect 3's `Either` is value-first — `Either<A, E>`. The code
 * below uses the real Effect order, as `core/schema.ts` does.
 *
 * The rules themselves are carried over from the pre-port `examples/tic-tac-toe/functions.ts` unchanged, including
 * the order in which they reject (game over, then turn ownership, then the cell) and the fact that
 * the turn flips even on the move that ends the game.
 */
import { Either } from "effect";
import { RuleError } from "../../core/errors.ts";
import type { Body, Document } from "../../sdk/schema.ts";
import type { schema } from "./schema.ts";

export type Game = Document<typeof schema, "games">;
export type GameBody = Body<typeof schema, "games">;
export type MoveBody = Body<typeof schema, "moves">;
export type Mark = GameBody["turn"];
export type Board = GameBody["board"];

/**
 * `createGame`'s only rule, checked before any document exists: both names must be non-blank and
 * distinct. It lives here rather than in the handler so the handler stays plumbing (ADR 0002).
 */
export function validatePlayers(xPlayer: string, oPlayer: string): Either.Either<void, RuleError> {
  return !xPlayer.trim() || !oPlayer.trim() || xPlayer === oPlayer
    ? Either.left(new RuleError({ rule: "distinct-players", detail: "Provide two distinct players" }))
    : Either.right(undefined);
}

/** The eight winning lines, in the order the pre-port handler checked them. */
export const LINES: readonly (readonly [number, number, number])[] = [
  [0, 1, 2],
  [3, 4, 5],
  [6, 7, 8],
  [0, 3, 6],
  [1, 4, 7],
  [2, 5, 8],
  [0, 4, 8],
  [2, 4, 6],
];

/** The mark holding a complete line, or `null` if no line is held. */
export function winner(board: Board): Mark | null {
  for (const [a, b, c] of LINES) {
    const mark = board[a];
    if (mark && mark === board[b] && mark === board[c]) return mark;
  }
  return null;
}

/**
 * Plays `cell` for `playerId` on `game`, returning the complete next game body and the move to
 * record. Both are plain data: the caller decides how to persist them.
 *
 * `cell` is re-checked for integrality even though `v.integer()` already rejected non-integers at
 * the argument boundary, so that this function is total for any input a property test can invent.
 */
export function applyMove(
  game: Game,
  playerId: string,
  cell: number,
): Either.Either<{ game: GameBody; move: MoveBody }, RuleError> {
  if (game.status !== "playing") {
    return Either.left(new RuleError({ rule: "game-over", detail: "Game is over" }));
  }
  const expectedPlayer = game.turn === "X" ? game.xPlayer : game.oPlayer;
  if (playerId !== expectedPlayer) {
    return Either.left(new RuleError({ rule: "not-your-turn", detail: "Not your turn" }));
  }
  if (!Number.isInteger(cell) || cell < 0 || cell > 8 || game.board[cell] !== null) {
    return Either.left(new RuleError({ rule: "invalid-move", detail: "Invalid move", fields: { cell } }));
  }
  const board = [...game.board];
  board[cell] = game.turn;
  const champion = winner(board);
  const filled = board.filter((mark) => mark !== null).length;
  return Either.right({
    game: {
      xPlayer: game.xPlayer,
      oPlayer: game.oPlayer,
      board,
      // The turn flips even when the game ends; `status` is what stops further moves.
      turn: game.turn === "X" ? "O" : "X",
      status: champion ? "won" : filled === board.length ? "draw" : "playing",
      winner: champion,
    },
    move: { gameId: game._id, playerId, mark: game.turn, cell, moveNumber: filled },
  });
}
