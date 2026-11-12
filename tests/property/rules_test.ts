/**
 * Property tests for `examples/tic-tac-toe/rules.ts` (plan item 12, work 3).
 *
 * States are *reached by play*, never fabricated: a game is built by applying a generated move order
 * through `applyMove`, so "reachable board" means what it says. The win oracle (`holdsLine`) spells the
 * eight lines out by arithmetic rather than importing `LINES`, so a wrong `LINES` table is caught.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import fc from "fast-check";
import { Either } from "effect";
import { applyMove, type Game, type Mark, winner } from "../../examples/tic-tac-toe/rules.ts";
import { check, SEED, titled } from "./arbitraries.ts";

type Board = Game["board"];

const CELLS = [0, 1, 2, 3, 4, 5, 6, 7, 8];
const filled = (board: readonly (Mark | null)[]): number => board.filter((c) => c !== null).length;

/** Independent win oracle: rows, columns and both diagonals by arithmetic. */
function holdsLine(board: readonly (Mark | null)[]): Mark | null {
  const lines: number[][] = [];
  for (let i = 0; i < 3; i++) {
    lines.push([3 * i, 3 * i + 1, 3 * i + 2]); // row i
    lines.push([i, i + 3, i + 6]); // column i
  }
  lines.push([0, 4, 8], [2, 4, 6]);
  for (const [a, b, c] of lines) {
    if (board[a] !== null && board[a] === board[b] && board[b] === board[c]) return board[a];
  }
  return null;
}

function newGame(xPlayer: string, oPlayer: string): Game {
  return {
    _id: "game-1",
    _creationTime: 1_700_000_000_000,
    _version: 1,
    xPlayer,
    oPlayer,
    board: Array(9).fill(null) as Board,
    turn: "X",
    status: "playing",
    winner: null,
  };
}
const toMove = (g: Game): string => (g.turn === "X" ? g.xPlayer : g.oPlayer);

/** One applied move folded back into a stored-document-shaped game, as `examples/tic-tac-toe/functions.ts` does. */
function step(game: Game, cell: number): Game | null {
  const result = applyMove(game, toMove(game), cell);
  if (Either.isLeft(result)) return null;
  return { ...game, ...result.right.game, _version: game._version + 1 };
}

/** Plays `order` until the game ends or the order runs out. Every move in a permutation is legal until then. */
function play(start: Game, order: readonly number[]): { game: Game; history: Game[] } {
  let game = start;
  const history: Game[] = [game];
  for (const cell of order) {
    if (game.status !== "playing") break;
    const next = step(game, cell);
    if (next === null) assert.fail(`legal move rejected: cell ${cell} on ${JSON.stringify(game.board)}`);
    game = next;
    history.push(game);
  }
  return { game, history };
}

const playersArb = fc.tuple(
  fc.string({ minLength: 1, maxLength: 8 }),
  fc.string({ minLength: 1, maxLength: 8 }),
)
  .filter(([x, o]) => x.trim() !== "" && o.trim() !== "" && x !== o);
/** A move order: some prefix of a permutation of the nine cells, so every move is to an empty cell. */
const orderArb = fc.shuffledSubarray(CELLS, { minLength: 0, maxLength: 9 }).chain((cells) =>
  fc.shuffledSubarray(cells, { minLength: cells.length, maxLength: cells.length })
);
const reachableArb = fc.tuple(playersArb, orderArb).map(([[x, o], order]) => play(newGame(x, o), order));
const boardArb = fc.array(fc.constantFrom<Mark | null>("X", "O", null), { minLength: 9, maxLength: 9 });

// Draw reachability must not depend on sampling rare full orders (sq-11 review, P7).
test("a full-board draw is reachable by legal moves", () => {
  const { game, history } = play(newGame("x", "o"), [0, 1, 2, 4, 3, 5, 7, 6, 8]);
  assert.equal(history.length, 10, "all nine moves were applied");
  assert.ok(history.slice(0, -1).every((g) => g.status === "playing"));
  assert.equal(filled(game.board), 9);
  assert.equal(game.status, "draw");
  assert.equal(game.winner, null);
  assert.equal(holdsLine(game.board), null);
});

test(titled("generators: reachable states cover playing and won, arbitrary boards cover lines"), () => {
  const states = fc.sample(reachableArb, { numRuns: 300, seed: SEED }).map((s) => s.game);
  assert.ok(states.some((g) => g.status === "playing" && filled(g.board) > 0), "no mid-game state");
  assert.ok(states.some((g) => g.status === "won"), "no won state");
  const boards = fc.sample(boardArb, { numRuns: 300, seed: SEED });
  assert.ok(boards.some((b) => holdsLine(b) !== null) && boards.some((b) => holdsLine(b) === null));
});

test(
  titled("a legal move fills exactly one empty cell with the mover's mark and nothing else changes"),
  () => {
    check(fc.property(reachableArb, ({ game }) => {
      fc.pre(game.status === "playing");
      for (const cell of CELLS.filter((c) => game.board[c] === null)) {
        const result = applyMove(game, toMove(game), cell);
        assert.ok(Either.isRight(result), `cell ${cell} is empty and it is the mover's turn`);
        if (Either.isLeft(result)) return;
        const { game: next, move } = result.right;
        assert.equal(filled(next.board), filled(game.board) + 1);
        assert.equal(next.board[cell], game.turn);
        for (const other of CELLS.filter((c) => c !== cell)) {
          assert.equal(next.board[other], game.board[other]);
        }
        assert.equal(next.turn, game.turn === "X" ? "O" : "X");
        assert.equal(move.cell, cell);
        assert.equal(move.mark, game.turn);
        assert.equal(move.moveNumber, filled(next.board));
      }
    }));
  },
);

test(titled("illegal moves are rejected and the input game is untouched"), () => {
  check(fc.property(
    reachableArb,
    fc.oneof(fc.integer({ min: -3, max: 12 }), fc.double(), fc.constantFrom(NaN, Infinity, -Infinity, 0.5)),
    (reached, cell) => {
      const { game } = reached;
      const before = structuredClone(game);
      const result = applyMove(game, toMove(game), cell);
      const legal = game.status === "playing" && Number.isInteger(cell) && cell >= 0 && cell <= 8 &&
        game.board[cell] === null;
      assert.equal(Either.isRight(result), legal);
      if (Either.isLeft(result) && game.status === "playing") assert.equal(result.left.rule, "invalid-move");
      // The mover who does not own the turn is always refused while the game is live.
      if (game.status === "playing") {
        const other = game.turn === "X" ? game.oPlayer : game.xPlayer;
        const wrong = applyMove(game, other, Number.isInteger(cell) ? cell : 0);
        assert.ok(Either.isLeft(wrong) && wrong.left.rule === "not-your-turn");
      }
      assert.deepStrictEqual(game, before, "applyMove must not mutate its input");
    },
  ));
});

test(titled("a won game stays won: every further move is rejected as game-over"), () => {
  check(fc.property(reachableArb, ({ game, history }) => {
    fc.pre(game.status === "won");
    for (const player of [game.xPlayer, game.oPlayer]) {
      for (const cell of CELLS) {
        const result = applyMove(game, player, cell);
        assert.ok(Either.isLeft(result) && result.left.rule === "game-over");
      }
    }
    // And it was never won earlier in the history: the winning move is the last one.
    assert.ok(history.slice(0, -1).every((g) => g.status === "playing"));
  }));
});

test(titled("winner is non-null iff some line is uniform and non-null (any board)"), () => {
  check(fc.property(boardArb, (board) => {
    assert.equal(winner(board as Board), holdsLine(board));
  }));
});

test(titled("status agrees with the board: won iff a line is held, a full board is never playing"), () => {
  check(fc.property(reachableArb, ({ game }) => {
    const held = holdsLine(game.board);
    assert.equal(game.winner, held);
    assert.equal(game.status === "won", held !== null);
    if (filled(game.board) === 9) assert.ok(game.status === "draw" || game.status === "won");
    if (game.status === "draw") assert.ok(filled(game.board) === 9 && held === null);
    if (game.status === "playing") assert.ok(filled(game.board) < 9 && held === null);
  }));
});

test(titled("replaying the same move sequence is deterministic"), () => {
  check(fc.property(playersArb, orderArb, ([x, o], order) => {
    const first = play(newGame(x, o), order);
    const second = play(newGame(x, o), order);
    assert.deepStrictEqual(second, first);
    // Moves are recorded in order with consecutive move numbers.
    let game = newGame(x, o);
    const numbers: number[] = [];
    for (const cell of order) {
      if (game.status !== "playing") break;
      const result = applyMove(game, toMove(game), cell);
      if (Either.isLeft(result)) return assert.fail("legal move rejected");
      numbers.push(result.right.move.moveNumber);
      game = { ...game, ...result.right.game };
    }
    assert.deepStrictEqual(numbers, numbers.map((_, i) => i + 1));
  }));
});
