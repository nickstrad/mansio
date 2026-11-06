// Smoke scenario: one VU replays the sample app's call order (createGame, getGame, makeMove x5, winsByMark)
// against a live agentd.
// Thresholds double as the "fail loudly if the server is down" mechanism: a down server produces
// connection-refused requests, which k6 always counts in http_req_failed regardless of status
// expectations, so the threshold fails and the run exits non-zero.
import { check, fail, sleep } from "k6";
import exec from "k6/execution";
import { mutation, query, RUN_ID } from "./lib/api.js";

export const options = {
  vus: 1,
  duration: "30s",
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: ["p(95)<250"],
    checks: ["rate==1.0"],
  },
};

export default function () {
  const xPlayer = `alice-${__VU}-${__ITER}`;
  const oPlayer = `bob-${__VU}-${__ITER}`;

  const created = mutation("createGame", { xPlayer, oPlayer });
  const createOk = check(created, {
    "createGame: status 200": (r) => r.res.status === 200,
    "createGame: has gameId": (r) => Boolean(r.body?.result?.gameId),
  });
  if (!createOk) {
    // Fail fast: a down server must abort the run, not spin hot hammering it for 30s.
    exec.test.abort(`createGame failed: status=${created.res.status} body=${created.res.body}`);
  }
  const gameId = created.body.result.gameId;

  const fetched = query("getGame", { gameId });
  const getOk = check(fetched, {
    "getGame: status 200": (r) => r.res.status === 200,
    "getGame: has _version": (r) => typeof r.body?.result?._version === "number",
  });
  if (!getOk) fail(`getGame failed: status=${fetched.res.status} body=${fetched.res.body}`);
  let game = fetched.body.result;

  const moves = [
    [xPlayer, 0],
    [oPlayer, 3],
    [xPlayer, 1],
    [oPlayer, 4],
    [xPlayer, 2],
  ];
  for (const [playerId, cell] of moves) {
    const moved = mutation("makeMove", { gameId, playerId, cell, expectedVersion: game._version });
    const moveOk = check(moved, {
      "makeMove: status 200": (r) => r.res.status === 200,
      "makeMove: has result": (r) => Boolean(r.body?.result),
    });
    if (!moveOk) fail(`makeMove failed: status=${moved.res.status} body=${moved.res.body}`);
    game = moved.body.result;
  }

  const wins = query("winsByMark", {});
  const winsOk = check(wins, {
    "winsByMark: status 200": (r) => r.res.status === 200,
    "winsByMark: has result": (r) => r.body?.result !== undefined,
  });
  if (!winsOk) fail(`winsByMark failed: status=${wins.res.status} body=${wins.res.body}`);

  sleep(1);
}

export function handleSummary(data) {
  // Hand-rolled verdicts: per-threshold ok booleans plus check pass/fail counts.
  const thresholds = {};
  for (const [name, metric] of Object.entries(data.metrics)) {
    if (metric.thresholds) {
      thresholds[name] = {};
      for (const [expr, t] of Object.entries(metric.thresholds)) {
        thresholds[name][expr] = t.ok;
      }
    }
  }
  return {
    [`logs/k6/${RUN_ID}/summary.json`]: JSON.stringify(data, null, 2),
    stdout: JSON.stringify(
      {
        run_id: RUN_ID,
        thresholds,
        checks: {
          passes: data.metrics.checks?.values.passes,
          fails: data.metrics.checks?.values.fails,
        },
        http_req_failed_rate: data.metrics.http_req_failed?.values.rate,
        http_req_duration_p95: data.metrics.http_req_duration?.values["p(95)"],
      },
      null,
      2,
    ),
  };
}
