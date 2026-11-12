// Shared k6 helpers for calling the document-db HTTP API: agentd's POST /v1/ops, the handlers sent as the
// SDK sends them (./ir.js; tickets and the schema fingerprint from READ_TICKET,
// WRITE_TICKET, SCHEMA_FINGERPRINT).
import http from "k6/http";
import { irCall } from "./ir.js";

// Treat any non-200 API response as a failure (k6 counts it in http_req_failed).
// Set here so every script importing these helpers inherits the 200-only callback.
http.setResponseCallback(http.expectedStatuses(200));

export const BASE_URL = __ENV.BASE_URL ?? "http://127.0.0.1:7070";
export const APP_ID = __ENV.APP_ID ?? "tic-tac-toe";
// Predictable default so the caller always knows the artifact dir: logs/k6/<RUN_ID>/.
export const RUN_ID = __ENV.K6_RUN_ID ?? "k6-smoke";

function traceId() {
  return `${__VU}-${__ITER}-${RUN_ID}`;
}

function call(_kind, name, args) {
  return irCall({ base: BASE_URL, app: APP_ID, traceId: traceId(), runId: RUN_ID }, name, args);
}

export function query(name, args) {
  return call("query", name, args);
}

export function mutation(name, args) {
  return call("mutation", name, args);
}
