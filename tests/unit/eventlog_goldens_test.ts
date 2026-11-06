/**
 * The event-log goldens shared with Go (`server/internal/eventlog/testdata/goldens.jsonl`, IR-4c): one
 * `{name, line}` per record, `line` being `toJsonLine`'s output. agentd's encoder must write the same bytes
 * for the same record (`record_test.go` reads the same file), so a change to the envelope, key order,
 * redaction or number format on either side fails a test.
 *
 * Regenerate after a deliberate change: `GOLDENS_WRITE=1 deno test -A tests/unit/eventlog_goldens_test.ts`.
 */
import assert from "node:assert/strict";
import {
  backupEvent,
  type EventRecord,
  handlerEvent,
  httpEvent,
  lifecycleEvent,
  queueEvent,
  sqlEvent,
  toJsonLine,
  txEvent,
} from "../../core/events.ts";

const FILE = new URL("../../server/internal/eventlog/testdata/goldens.jsonl", import.meta.url);
const env = {
  eventId: "e1",
  ts: "2026-11-06T10:11:12.345Z",
  level: "info" as const,
  role: "server" as const,
  pid: 42,
  traceId: "t-1",
};

/** The records, by name. `record_test.go` builds the same ones under the same names. */
const records: Record<string, EventRecord> = {
  "http with redacted detail": httpEvent({
    ...env,
    level: "warn",
    appId: "tic-tac-toe",
    method: "POST",
    path: "/v1/ops",
    handlerKind: "mutation",
    name: "makeMove",
    status: 409,
    durationMs: 1.5,
    bytesIn: 120,
    bytesOut: 77,
    errorTag: "ConflictError",
    errorDetail: {
      tag: "ConflictError",
      message: "line1\nline2 Bearer abc.def",
      fields: { Authorization: "Bearer x", "x-api-token": "s", nested: [1, "Bearer zz", null, true] },
    },
  }),
  "handler": handlerEvent({
    ...env,
    spanId: "s1",
    parentSpanId: "p1",
    appId: "a",
    handlerKind: "query",
    name: "getGame",
    outcome: "ok",
    durationMs: 0.25,
  }),
  "sql never carries bind values": sqlEvent({
    ...env,
    spanId: "s1",
    appId: "a",
    engine: "sqlite",
    dialect: "sqlite",
    statementKind: "select",
    tableName: "games",
    sqlText: 'SELECT * FROM "games" WHERE "_id" = ?',
    bindCount: 1,
    bindValues: ["secret"],
    durationMs: 0.012,
    rowCount: 1,
    outcome: "ok",
  }),
  "tx": txEvent({
    ...env,
    spanId: "s1",
    appId: "a",
    engine: "sqlite",
    outcome: "rollback",
    operations: 3,
    durationMs: 2,
    queueWaitMs: 0.5,
    errorTag: "StorageError",
    errorDetail: { reason: "x" },
  }),
  "queue drop report": queueEvent({
    ...env,
    level: "warn",
    traceId: "event-sink",
    queue: "events",
    depth: 10,
    cap: 10,
    event: "dropped",
    dropped: 3,
  }),
  "lifecycle with S3 secrets": lifecycleEvent({
    ...env,
    phase: "startup",
    detail: {
      url: "https://s3/x?X-Amz-Signature=abc&X-Amz-Credential=def&ok=1",
      AWS_SECRET_ACCESS_KEY: "k",
      sessionToken: "t",
      list: ["a b", { Cookie: "c" }],
    },
  }),
  "backup with absent optionals": backupEvent({
    ...env,
    appId: "a",
    phase: "publish-latest",
    emitter: "backups",
    runId: "r1",
    durationMs: 3,
    outcome: "ok",
  }),
  // JavaScript's regexp and string semantics, which Go has to spell out (review M1, L2).
  "lifecycle with JS whitespace, case folding, key order and numbers": lifecycleEvent({
    ...env,
    phase: "js-semantics",
    detail: {
      lineSeparator: "Bearer SECRET1",
      nbsp: "Bearer SECRET2",
      verticalTab: "Bearer\vSECRET3",
      bom: "Bearer﻿SECRET4",
      ideographic: "Bearer　SECRET5",
      amzNbsp: "u?X-Amz-Signature=x kept",
      longS: "X-Amz-ſignature=kept",
      ["COOKIE"]: "kelvin sign lower-cases to k",
      "｡": "sorts after",
      "\u{10000}": "sorts before (UTF-16)",
      big: 1e21,
      small: 1e-7,
      plain: 123.456,
    },
  }),
};

Deno.test("eventlog goldens: toJsonLine writes the lines agentd's encoder is held to", async () => {
  const lines = Object.entries(records).map(([name, record]) =>
    JSON.stringify({ name, line: toJsonLine(record) })
  );
  if (Deno.env.get("GOLDENS_WRITE") === "1") await Deno.writeTextFile(FILE, lines.join("\n") + "\n");
  assert.deepEqual((await Deno.readTextFile(FILE)).trimEnd().split("\n"), lines);
});
