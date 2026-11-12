/**
 * Unit tests for `examples/tic-tac-toe/rules.ts` (plan item 10).
 *
 * The point of these is that the game is now testable without a database: no `SQLiteWriter`, no
 * temp directory, no transaction. Every case the pre-port handler covered by raising is asserted
 * here on the returned `RuleError` instead, plus the win/draw detection that `tests/core_test.ts`
 * could only observe through a committed game. The property-based version lands in item 12.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { Either } from "effect";
import { RuleError } from "../../core/errors.ts";
import {
  applyMove,
  type Game,
  type GameBody,
  LINES,
  type Mark,
  type MoveBody,
  validatePlayers,
  winner,
} from "../../examples/tic-tac-toe/rules.ts";

/** A fresh, playable game. `_id`/`_creationTime`/`_version` stand in for stored metadata. */
function game(overrides: Partial<Game> = {}): Game {
  return {
    _id: "game-1",
    _creationTime: 1_700_000_000_000,
    _version: 1,
    xPlayer: "alice",
    oPlayer: "bob",
    board: Array(9).fill(null) as Game["board"],
    turn: "X",
    status: "playing",
    winner: null,
    ...overrides,
  };
}
const playerOf = (state: Game): string => (state.turn === "X" ? state.xPlayer : state.oPlayer);

function expectRight<A>(either: Either.Either<A, RuleError>): A {
  if (Either.isRight(either)) return either.right;
  return assert.fail(`expected a right, got ${either.left.rule}: ${either.left.detail}`);
}
/** Asserts a rejection by rule, and that its detail is the text the HTTP body still reports. */
function expectRejection(either: Either.Either<unknown, RuleError>, rule: string, detail: string): void {
  if (Either.isRight(either)) return assert.fail(`expected a rejection, got ${JSON.stringify(either.right)}`);
  assert.equal(either.left._tag, "RuleError");
  assert.equal(either.left.rule, rule);
  assert.equal(either.left.detail, detail);
}

/**
 * Plays `cells` in order, alternating players from whoever has the turn, and folds each result
 * back into a stored document the way `examples/tic-tac-toe/functions.ts` does (patch the body, bump the version).
 */
function play(cells: readonly number[], start: Game = game()): { game: Game; moves: MoveBody[] } {
  let current = start;
  const moves: MoveBody[] = [];
  for (const cell of cells) {
    const next = expectRight(applyMove(current, playerOf(current), cell));
    moves.push(next.move);
    current = { ...current, ...next.game, _version: current._version + 1 };
  }
  return { game: current, moves };
}

// --- Winning -------------------------------------------------------------------------------

for (const [a, b, c] of LINES) {
  test(`applyMove: X wins on line ${a}-${b}-${c}`, () => {
    // X takes the line; O answers in the two lowest cells the line leaves free, so O never
    // completes anything of its own before X's third move.
    const free = [0, 1, 2, 3, 4, 5, 6, 7, 8].filter((cell) => ![a, b, c].includes(cell));
    const { game: finished, moves } = play([a, free[0], b, free[1], c]);
    assert.equal(finished.status, "won");
    assert.equal(finished.winner, "X");
    assert.equal(winner(finished.board), "X");
    assert.equal(moves.length, 5);
    assert.deepEqual(moves[4], { gameId: "game-1", playerId: "alice", mark: "X", cell: c, moveNumber: 5 });
  });
}

test("applyMove: a full board with no line is a draw, not a win", () => {
  const { game: finished } = play([0, 1, 2, 4, 3, 5, 7, 6, 8]);
  assert.equal(finished.status, "draw");
  assert.equal(finished.winner, null);
  assert.equal(winner(finished.board), null);
  assert.equal(finished.board.filter((mark) => mark !== null).length, 9);
});

test("applyMove: an unfinished game stays playing and flips the turn", () => {
  const first = expectRight(applyMove(game(), "alice", 4));
  assert.equal(first.game.status, "playing");
  assert.equal(first.game.winner, null);
  assert.equal(first.game.turn, "O");
  assert.deepEqual(first.move, { gameId: "game-1", playerId: "alice", mark: "X", cell: 4, moveNumber: 1 });
  const second = expectRight(applyMove({ ...game(), ...first.game, _version: 2 }, "bob", 0));
  assert.equal(second.game.turn, "X");
  assert.equal(second.move.moveNumber, 2);
  assert.deepEqual(second.game.board, ["O", null, null, null, "X", null, null, null, null]);
});

test("applyMove: the turn flips even on the move that ends the game", () => {
  const { game: finished } = play([0, 3, 1, 4, 2]);
  assert.equal(finished.status, "won");
  assert.equal(finished.turn, "O");
});

// --- Rejections ----------------------------------------------------------------------------

test("applyMove: an occupied cell is rejected", () => {
  const { game: afterX } = play([4]);
  expectRejection(applyMove(afterX, "bob", 4), "invalid-move", "Invalid move");
});

test("applyMove: a cell outside 0..8 is rejected", () => {
  for (const cell of [-1, 9, 10, 100, Number.MAX_SAFE_INTEGER]) {
    expectRejection(applyMove(game(), "alice", cell), "invalid-move", "Invalid move");
  }
});

test("applyMove: a non-integer cell is rejected without reading the board", () => {
  for (const cell of [0.5, Number.NaN, Number.POSITIVE_INFINITY]) {
    expectRejection(applyMove(game(), "alice", cell), "invalid-move", "Invalid move");
  }
});

test("applyMove: the player who does not hold the turn is rejected", () => {
  expectRejection(applyMove(game(), "bob", 0), "not-your-turn", "Not your turn");
  expectRejection(applyMove(game(), "carol", 0), "not-your-turn", "Not your turn");
  const { game: afterX } = play([0]);
  expectRejection(applyMove(afterX, "alice", 1), "not-your-turn", "Not your turn");
});

test("applyMove: a finished game rejects every further move", () => {
  const { game: won } = play([0, 3, 1, 4, 2]);
  expectRejection(applyMove(won, "bob", 5), "game-over", "Game is over");
  const { game: drawn } = play([0, 1, 2, 4, 3, 5, 7, 6, 8]);
  expectRejection(applyMove(drawn, "alice", 0), "game-over", "Game is over");
  // Game-over is checked before turn ownership and before the cell, as it was pre-port.
  expectRejection(applyMove(won, "nobody", 99), "game-over", "Game is over");
});

// --- Purity --------------------------------------------------------------------------------

test("applyMove: the game it was given is left untouched", () => {
  const before = game();
  const snapshot: GameBody = {
    xPlayer: before.xPlayer,
    oPlayer: before.oPlayer,
    board: [...before.board],
    turn: before.turn,
    status: before.status,
    winner: before.winner,
  };
  expectRight(applyMove(before, "alice", 0));
  assert.deepEqual(before.board, snapshot.board);
  assert.equal(before.turn, snapshot.turn);
  assert.equal(before.status, snapshot.status);
});

// --- winner --------------------------------------------------------------------------------

test("winner: reports the mark holding a complete line, for each line and each mark", () => {
  for (const mark of ["X", "O"] as Mark[]) {
    const other: Mark = mark === "X" ? "O" : "X";
    for (const [a, b, c] of LINES) {
      const board: Game["board"] = Array(9).fill(null) as Game["board"];
      board[a] = board[b] = board[c] = mark;
      assert.equal(winner(board), mark);
      // The loser holding two cells of some other line changes nothing.
      const free = [0, 1, 2, 3, 4, 5, 6, 7, 8].filter((cell) => ![a, b, c].includes(cell));
      board[free[0]] = board[free[1]] = other;
      assert.equal(winner(board), mark);
    }
  }
});

test("winner: an empty, partial or line-free board has no winner", () => {
  assert.equal(winner(Array(9).fill(null) as Game["board"]), null);
  assert.equal(winner(["X", "X", null, null, "O", null, null, null, null]), null);
  assert.equal(winner(["X", "O", "X", "X", "O", "O", "O", "X", "X"]), null);
});

test("validatePlayers: blank or identical players are rejected, distinct players pass", () => {
  assert.ok(Either.isRight(validatePlayers("alice", "bob")));
  for (const [xPlayer, oPlayer] of [["", "bob"], ["alice", ""], ["  ", "bob"], ["alice", "alice"]] as const) {
    const result = validatePlayers(xPlayer, oPlayer);
    assert.ok(Either.isLeft(result));
    if (Either.isLeft(result)) {
      assert.equal(result.left._tag, "RuleError");
      assert.equal(result.left.rule, "distinct-players");
      assert.equal(result.left.detail, "Provide two distinct players");
    }
  }
});
