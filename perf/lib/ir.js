// The tic-tac-toe handlers over the IR protocol (POST /v1/ops; ir/spec/v1.md), for k6 runs against agentd
// (the only backend since IR-6). k6 cannot run the TS SDK, so each handler here sends what sdk/client.ts sends for it (the
// requests the `sdk:` vectors in ir/vectors/execute.json pin): a query is one read request; makeMove reads the
// game, applies app/rules.ts locally and commits [assert version, patch game, insert move]. A rule failure or
// an app conflict (stale expectedVersion) fails locally, as in the SDK, without a request; its answer is
// synthetic (`res.status` 400/409, no k6 metric).
//
// Answers keep the platform API's shape (`{result}` or `{error: {tag}}`), so the scenarios do not change.
// Every request carries X-Trace-Id, X-Mansio-Handler (the event log's handler name) and a ticket:
//
//   READ_TICKET, WRITE_TICKET  tickets for TENANT:APP_ID (minted by the caller: sdk/ticket.ts)
//   SCHEMA_FINGERPRINT         the app schema's fingerprint in its layout (sdk/fingerprint.ts)
//   TENANT                     default "local" (agentd's static mode serves local:<app>)
import http from "k6/http";

const READ_TICKET = __ENV.READ_TICKET ?? "";
const WRITE_TICKET = __ENV.WRITE_TICKET ?? "";
const SCHEMA = __ENV.SCHEMA_FINGERPRINT ?? "";
const TENANT = __ENV.TENANT ?? "local";

let sent = 0; // per VU: makes request ids unique within the run

function send(ctx, kind, handler, ops) {
  const body = { v: 1, db: { tenant: TENANT, app: ctx.app }, kind, schema: SCHEMA, ops };
  if (kind === "mutation") body.requestId = `${ctx.runId}-${__VU}-${__ITER}-${++sent}`.slice(-64);
  const res = http.post(`${ctx.base}/v1/ops`, JSON.stringify(body), {
    headers: {
      "content-type": "application/json",
      "authorization": `Bearer ${kind === "query" ? READ_TICKET : WRITE_TICKET}`,
      "x-trace-id": ctx.traceId,
      "x-mansio-handler": handler,
    },
    tags: { name: handler },
  });
  let parsed;
  try {
    parsed = res.json();
  } catch (_e) {
    parsed = undefined;
  }
  return { res, body: parsed };
}

// A handler's answer from an IR answer: `map` turns the result array into the handler's result.
function answer(r, map) {
  if (r.res.status !== 200 || !Array.isArray(r.body?.result)) return r;
  return { res: r.res, body: { result: map(r.body.result) } };
}

// A local failure, as the SDK reports it (no request was sent).
function local(status, tag, message) {
  const body = { error: { tag, message } };
  return { res: { status, body: JSON.stringify(body) }, body };
}

const LINES = [[0, 1, 2], [3, 4, 5], [6, 7, 8], [0, 3, 6], [1, 4, 7], [2, 5, 8], [0, 4, 8], [2, 4, 6]];

function winner(board) {
  for (const [a, b, c] of LINES) {
    if (board[a] && board[a] === board[b] && board[a] === board[c]) return board[a];
  }
  return null;
}

const read = (ctx, handler, op) => answer(send(ctx, "query", handler, [op]), (result) => result[0]);
const eq = (field, value) => ({ field, op: "eq", value });

const handlers = {
  createGame(ctx, { xPlayer, oPlayer }) {
    if (!xPlayer.trim() || !oPlayer.trim() || xPlayer === oPlayer) {
      return local(400, "RuleError", "Provide two distinct players");
    }
    const value = {
      xPlayer,
      oPlayer,
      board: Array(9).fill(null),
      turn: "X",
      status: "playing",
      winner: null,
    };
    return answer(
      send(ctx, "mutation", "createGame", [{ op: "insert", table: "games", value }]),
      (result) => ({ gameId: result[0] }),
    );
  },
  getGame: (ctx, { gameId }) => read(ctx, "getGame", { op: "get", table: "games", id: gameId }),
  listGames: (ctx, { status }) =>
    read(ctx, "listGames", {
      op: "select",
      table: "games",
      filters: [eq("status", status)],
      order: { field: "_creationTime", direction: "desc" },
      limit: 20,
    }),
  moveHistory: (ctx, { gameId }) =>
    read(ctx, "moveHistory", {
      op: "select",
      table: "moves",
      filters: [eq("gameId", gameId)],
      order: { field: "moveNumber", direction: "asc" },
      limit: 9,
    }),
  winsByMark: (ctx) =>
    read(ctx, "winsByMark", {
      op: "select",
      table: "games",
      filters: [eq("status", "won")],
      aggregate: { kind: "countBy", field: "winner" },
      limit: 100,
    }),
  makeMove(ctx, { gameId, playerId, cell, expectedVersion }) {
    const got = send(ctx, "query", "makeMove", [{ op: "get", table: "games", id: gameId }]);
    if (got.res.status !== 200) return got;
    const game = got.body.result[0];
    if (!game) return local(404, "NotFoundError", `games ${gameId} not found`);
    if (game._version !== expectedVersion) return local(409, "ConflictError", "version mismatch");
    if (game.status !== "playing") return local(400, "RuleError", "Game is over");
    if (playerId !== (game.turn === "X" ? game.xPlayer : game.oPlayer)) {
      return local(400, "RuleError", "Not your turn");
    }
    if (!Number.isInteger(cell) || cell < 0 || cell > 8 || game.board[cell] !== null) {
      return local(400, "RuleError", "Invalid move");
    }
    const board = [...game.board];
    board[cell] = game.turn;
    const champion = winner(board);
    const filled = board.filter((mark) => mark !== null).length;
    const next = {
      xPlayer: game.xPlayer,
      oPlayer: game.oPlayer,
      board,
      turn: game.turn === "X" ? "O" : "X",
      status: champion ? "won" : filled === board.length ? "draw" : "playing",
      winner: champion,
    };
    const move = { gameId, playerId, mark: game.turn, cell, moveNumber: filled };
    return answer(
      send(ctx, "mutation", "makeMove", [
        { op: "assert", table: "games", id: gameId, expect: { version: game._version } },
        { op: "patch", table: "games", id: gameId, value: next },
        { op: "insert", table: "moves", value: move },
      ]),
      // What the SDK answers: the handler's final read, from its cache with the patch applied.
      () => ({ ...game, ...next, _version: game._version + 1 }),
    );
  },
};

// One handler call: `{res, body}` with `body` `{result}` or `{error: {tag}}`.
export function irCall(ctx, name, args) {
  const handler = handlers[name];
  if (!handler) return local(404, "UnknownHandlerError", `no handler ${name}`);
  return handler(ctx, args);
}
