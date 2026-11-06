// The SDK's promise wrapper against the Deno server's `/v1/ops` shim (IR-3): the handlers run in this
// process. This script is unowned by any plan item.
import { createPromiseClient } from "../sdk/client.ts";
import { functions } from "../app/functions.ts";
import { schema } from "../app/schema.ts";
import { withStorage } from "../sdk/schema.ts";
const base = Deno.env.get("BASE_URL") ?? "http://127.0.0.1:8000";
const app = Deno.args[0] ?? "tic-tac-toe";
const client = createPromiseClient({
  functions,
  // The database's layout is part of its fingerprint: the json app stores documents as one JSON column.
  schema: withStorage(schema, app === "tic-tac-toe-json" ? "json" : "columns"),
  tenant: "local",
  app,
  endpoint: `${base}/apps/${app}`,
  token: Deno.env.get("TOKEN") ?? "dev-only",
});
const { gameId } = await client.mutation("createGame", { xPlayer: "alice", oPlayer: "bob" });
console.log("Created:", gameId);
let game = await client.query("getGame", { gameId });
for (const [playerId, cell] of [["alice", 0], ["bob", 3], ["alice", 1], ["bob", 4], ["alice", 2]] as const) {
  game = await client.mutation("makeMove", { gameId, playerId, cell, expectedVersion: game!._version });
}
console.log("Game:", await client.query("getGame", { gameId }));
console.log("Moves:", await client.query("moveHistory", { gameId }));
console.log("Wins:", await client.query("winsByMark", {}));
console.log("Won games:", await client.query("listGames", { status: "won" }));
