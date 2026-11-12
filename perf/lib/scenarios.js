// Shared pieces for the load scenarios (mutation_load.js, query_load.js, mixed.js): seeding, the
// weighted read mix, conflict-aware writes, the run id and artifact paths, and handleSummary.
// HTTP calls go through ./api.js, which owns the route shape and the X-Trace-Id header.
import http from "k6/http";
import { check } from "k6";
import { Counter } from "k6/metrics";
import { mutation, query, RUN_ID } from "./api.js";

// A stale-version write that the server rejects. Counted here instead of in http_req_failed, which
// is why `staleMove` widens the expected statuses for that one request.
const conflicts = new Counter("conflicts");

// Artifacts. k6 writes raw.json only when started with `--out json=<RAW_PATH>` (or K6_OUT), and
// creates neither directory; the caller mkdirs OUT_DIR. See perf/README.md.
const OUT_DIR = `logs/k6/${RUN_ID}`;
const RAW_PATH = `${OUT_DIR}/raw.json`;
const SUMMARY_PATH = `${OUT_DIR}/summary.json`;

// `_version` of a freshly inserted document (createGame returns only the id).
const INITIAL_VERSION = 1;

// Cell sequences played alternately X, O, X, ...
export const GAME_SCRIPTS = {
  xWins: { cells: [0, 3, 1, 4, 2] },
  oWins: { cells: [0, 3, 1, 4, 8, 5] },
  draw: { cells: [0, 1, 2, 4, 3, 5, 7, 6, 8] },
  playing: { cells: [0, 3] },
};

function failureText(r) {
  return `status=${r.res.status} body=${r.res.body}`;
}

// Runs fn with `statuses` as the expected response statuses for requests it makes, then restores
// the 200-only default that perf/lib/api.js installs.
function withExpectedStatuses(statuses, fn) {
  http.setResponseCallback(http.expectedStatuses(...statuses));
  try {
    return fn();
  } finally {
    http.setResponseCallback(http.expectedStatuses(200));
  }
}

export function createGame(label) {
  const r = mutation("createGame", { xPlayer: `x-${label}`, oPlayer: `o-${label}` });
  const ok = check(r, {
    "createGame: status 200": (x) => x.res.status === 200,
    "createGame: has gameId": (x) => Boolean(x.body?.result?.gameId),
  });
  return ok ? { r, game: newGameState(r.body.result.gameId, label) } : { r, game: undefined };
}

function newGameState(gameId, label) {
  return { gameId, label, version: INITIAL_VERSION, played: 0 };
}

// Plays the next cell of `cells` on `game` (mutating game.version/played). Returns the response, or
// undefined when the script is exhausted.
export function playNext(game, cells) {
  if (game.played >= cells.length) return undefined;
  const mark = game.played % 2 === 0 ? "x" : "o";
  const r = mutation("makeMove", {
    gameId: game.gameId,
    playerId: `${mark}-${game.label}`,
    cell: cells[game.played],
    expectedVersion: game.version,
  });
  const ok = check(r, {
    "makeMove: status 200": (x) => x.res.status === 200,
    "makeMove: has version": (x) => typeof x.body?.result?._version === "number",
  });
  if (ok) {
    game.version = r.body.result._version;
    game.played += 1;
  }
  return r;
}

// True for a stale-version rejection: 409 from a server that maps ConflictError, or the
// 400 `{"error":"ConflictError"}` a server that does not map it returns.
function isConflict(r) {
  if (r.res.status === 409) return true;
  return r.res.status === 400 && String(r.body?.error?.tag ?? r.body?.error).includes("ConflictError");
}

// Sends the next move with a deliberately stale expectedVersion. The server must reject it; that
// is a success for the scenario, recorded in the `conflicts` counter.
export function staleMove(game, cells) {
  if (game.played >= cells.length) return;
  const mark = game.played % 2 === 0 ? "x" : "o";
  const r = withExpectedStatuses([400, 409], () =>
    mutation("makeMove", {
      gameId: game.gameId,
      playerId: `${mark}-${game.label}`,
      cell: cells[game.played],
      expectedVersion: game.version - 1,
    }));
  const conflict = isConflict(r);
  if (conflict) conflicts.add(1);
  check(r, { "stale makeMove: rejected as conflict": () => conflict });
}

// Creates `count` games cycling through GAME_SCRIPTS (weighted toward finished games so listGames
// and winsByMark have something to return). Run from setup(); throws so k6 aborts before the load.
export function seedGames(count) {
  // api.js builds its trace id from __VU and __ITER; k6 leaves __ITER undefined inside setup().
  if (typeof __ITER === "undefined") globalThis.__ITER = 0;
  const order = ["xWins", "xWins", "oWins", "draw", "playing"];
  const seeded = [];
  for (let i = 0; i < count; i++) {
    const kind = order[i % order.length];
    const { r, game } = createGame(`seed-${i}`);
    if (!game) throw new Error(`seed: createGame failed: ${failureText(r)}`);
    const { cells } = GAME_SCRIPTS[kind];
    while (game.played < cells.length) {
      const moved = playNext(game, cells);
      if (moved.res.status !== 200) throw new Error(`seed: makeMove failed: ${failureText(moved)}`);
    }
    seeded.push({ gameId: game.gameId });
  }
  return seeded;
}

// Read mix weighted 4:2:2:1 (getGame, listGames, moveHistory, winsByMark).
const READ_MIX = [
  ["getGame", 4],
  ["listGames", 2],
  ["moveHistory", 2],
  ["winsByMark", 1],
];
const READ_TOTAL = READ_MIX.reduce((n, [, w]) => n + w, 0);
const STATUSES = ["playing", "won", "draw"];

function pickRead() {
  let n = Math.floor(Math.random() * READ_TOTAL);
  for (const [name, weight] of READ_MIX) {
    if (n < weight) return name;
    n -= weight;
  }
  return READ_MIX[0][0];
}

// One weighted-random read against the seeded games.
export function readOp(seeded) {
  const name = pickRead();
  const gameId = seeded[Math.floor(Math.random() * seeded.length)].gameId;
  const args = name === "getGame" || name === "moveHistory"
    ? { gameId }
    : name === "listGames"
    ? { status: STATUSES[Math.floor(Math.random() * STATUSES.length)] }
    : {};
  const r = query(name, args);
  check(r, {
    [`${name}: status 200`]: (x) => x.res.status === 200,
    [`${name}: has result`]: (x) => x.body?.result !== undefined,
  });
  return r;
}

// A scenario-owned handleSummary: summary.json under OUT_DIR plus a compact stdout verdict.
export function buildSummary(scenario, data) {
  const thresholds = {};
  for (const [name, metric] of Object.entries(data.metrics)) {
    if (metric.thresholds) {
      thresholds[name] = {};
      for (const [expr, t] of Object.entries(metric.thresholds)) thresholds[name][expr] = t.ok;
    }
  }
  const values = (name) => data.metrics[name]?.values ?? {};
  return {
    [SUMMARY_PATH]: JSON.stringify({ run_id: RUN_ID, scenario, ...data }, null, 2),
    stdout: JSON.stringify(
      {
        run_id: RUN_ID,
        scenario,
        artifacts: { raw: RAW_PATH, summary: SUMMARY_PATH },
        thresholds,
        checks: { passes: values("checks").passes, fails: values("checks").fails },
        http_reqs: values("http_reqs").count,
        http_req_failed_rate: values("http_req_failed").rate,
        http_req_duration_p95: values("http_req_duration")["p(95)"],
        conflicts: values("conflicts").count ?? 0,
        dropped_iterations: values("dropped_iterations").count ?? 0,
      },
      null,
      2,
    ) + "\n",
  };
}
