import { apps, assert, client, Effect, play, run, scenarioDir, startGoAgent, test } from "../harness.ts";
import { workloads } from "../seed.ts";

// Independent reference: derive status/winner from the moves sent, never from app rules or responses.
export function reference(cells: readonly number[]) {
  const board = Array<string | null>(9).fill(null);
  cells.forEach((cell, i) => {
    board[cell] = i % 2 ? "O" : "X";
  });
  const lines = [[0, 1, 2], [3, 4, 5], [6, 7, 8], [0, 3, 6], [1, 4, 7], [2, 5, 8], [0, 4, 8], [2, 4, 6]];
  const winner =
    lines.map(([a, b, c]) => board[a] && board[a] === board[b] && board[a] === board[c] ? board[a] : null)
      .find(Boolean) ?? null;
  return { board, winner, status: winner ? "won" : cells.length === 9 ? "draw" : "playing" };
}
function normalize(rows: readonly Record<string, unknown>[]) {
  return rows.map(({ _id: _i, _creationTime: _t, ...row }) => row)
    .sort((a, b) => String(a.xPlayer).localeCompare(String(b.xPlayer)));
}

test("e2e/2-queries: status, history and countBy match an independent seeded reference (SDK, /v1/ops)", () =>
  Effect.gen(function* () {
    const dir = yield* scenarioDir("queries");
    const server = yield* startGoAgent(dir);
    yield* Effect.promise(async () => {
      const results: unknown[] = [];
      for (const app of apps) {
        const c = client(server, app);
        const expected = [];
        for (const [i, cells] of workloads.entries()) {
          const game = await play(c, cells, `-${i}`);
          const ref = reference(cells);
          assert.equal(game.status, ref.status);
          assert.equal(game.winner, ref.winner);
          assert.deepEqual(game.board, ref.board);
          const history = await run(c.query("moveHistory", { gameId: game._id }));
          assert.equal(history.length, cells.length);
          assert.deepEqual(history.map((m) => m.cell), cells);
          assert.deepEqual(history.map((m) => m.moveNumber), cells.map((_, n) => n + 1));
          expected.push({ ...game, ...ref });
        }
        const lists = [];
        for (const status of ["playing", "won", "draw"] as const) {
          const rows = normalize(await run(c.query("listGames", { status })));
          assert.deepEqual(rows, normalize(expected.filter((g) => g.status === status)));
          lists.push(rows);
        }
        const counts = new Map<string, number>();
        for (const cells of workloads) {
          const ref = reference(cells);
          if (ref.winner) counts.set(ref.winner, (counts.get(ref.winner) ?? 0) + 1);
        }
        const wins = (await run(c.query("winsByMark", {}))).sort((a, b) =>
          String(a.value).localeCompare(String(b.value))
        );
        assert.deepEqual(wins, [...counts].sort().map(([value, count]) => ({ value, count })));
        results.push({ lists, wins });
      }
      assert.deepEqual(results[0], results[1], "columns/json results must agree");
    });
  }));
