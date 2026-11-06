/**
 * `eventlog/config.ts`, the event-log CLI's env boundary: defaults, typed rejections and path resolution.
 * Ported from the Deno platform's `tests/unit/config_test.ts` at IR-6 (the cases for the options that
 * remain: `DATA_DIR`, `LOG_*`, `TRACE_SQL`, `EVENT_*`, `EVENTLOG_DUCKDB`).
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { resolve } from "node:path";
import { Effect } from "effect";
import { Config } from "../../eventlog/services.ts";
import { ConfigLive, ConfigTest, OPTIONS, renderOptionsTable } from "../../eventlog/config.ts";

const ENV_VARS = OPTIONS.map((spec) => spec.envVar);

/** Clears every option's env var, applies `vars`, runs `run`, then restores the original environment. */
async function withEnv(vars: Readonly<Record<string, string | undefined>>, run: () => Promise<void>) {
  const saved = new Map(ENV_VARS.map((name) => [name, Deno.env.get(name)]));
  try {
    for (const name of ENV_VARS) Deno.env.delete(name);
    for (const [name, value] of Object.entries(vars)) {
      if (value === undefined) Deno.env.delete(name);
      else Deno.env.set(name, value);
    }
    await run();
  } finally {
    for (const [name, value] of saved) {
      if (value === undefined) Deno.env.delete(name);
      else Deno.env.set(name, value);
    }
  }
}

function load() {
  return Effect.runPromiseExit(Effect.provide(Config, ConfigLive));
}

async function loaded(vars: Readonly<Record<string, string | undefined>>) {
  let value: Effect.Effect.Success<typeof Config> | undefined;
  await withEnv(vars, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success", String(exit));
    if (exit._tag === "Success") value = exit.value;
  });
  return value!;
}

async function expectConfigError(vars: Record<string, string>, key: string, reason: string) {
  await withEnv(vars, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Failure");
    if (exit._tag !== "Failure") return;
    const error = exit.cause._tag === "Fail" ? exit.cause.error : undefined;
    assert.equal(error?._tag, "ConfigError");
    assert.equal(error?.key, key);
    assert.equal(error?.reason, reason);
  });
}

test("defaults: every option's documented default produces the matching ConfigService", async () => {
  const config = await loaded({});
  assert.deepEqual(config.logging, {
    dir: resolve("./logs"),
    level: "info",
    retentionDays: 30,
    gzipAfterDays: 7,
    maxTotalBytes: 1_073_741_824,
    maxFileBytes: 67_108_864,
    includeBindValues: false,
    traceSql: false,
    eventQueueCapacity: 10_000,
    eventFlushMs: 250,
  });
  assert.deepEqual(config.eventLog, { duckdbPath: resolve("./data", "eventlog.duckdb") });
});

test("empty-string env values are treated as unset, same as absent", async () => {
  const config = await loaded({ LOG_LEVEL: "", LOG_DIR: "", EVENTLOG_DUCKDB: "" });
  assert.equal(config.logging.level, "info");
  assert.equal(config.logging.dir, resolve("./logs"));
  assert.equal(config.eventLog.duckdbPath, resolve("./data", "eventlog.duckdb"));
});

test("an unrecognized LOG_LEVEL fails with ConfigError(malformed)", async () => {
  await expectConfigError({ LOG_LEVEL: "verbose" }, "LOG_LEVEL", "malformed");
});

test("non-numeric LOG_* and EVENT_* numbers fail with ConfigError(malformed)", async () => {
  for (
    const key of [
      "LOG_RETENTION_DAYS",
      "LOG_GZIP_AFTER_DAYS",
      "LOG_MAX_TOTAL_BYTES",
      "LOG_MAX_FILE_BYTES",
      "EVENT_QUEUE_CAPACITY",
      "EVENT_FLUSH_MS",
    ]
  ) {
    await expectConfigError({ [key]: "lots" }, key, "malformed");
  }
});

test("numeric LOG_* values decode", async () => {
  const config = await loaded({ LOG_RETENTION_DAYS: "3", LOG_MAX_FILE_BYTES: "1024", EVENT_FLUSH_MS: "50" });
  assert.equal(config.logging.retentionDays, 3);
  assert.equal(config.logging.maxFileBytes, 1024);
  assert.equal(config.logging.eventFlushMs, 50);
});

test("booleans outside true/false/1/0/yes/no fail with ConfigError(malformed)", async () => {
  await expectConfigError({ LOG_INCLUDE_BIND_VALUES: "maybe" }, "LOG_INCLUDE_BIND_VALUES", "malformed");
  await expectConfigError({ TRACE_SQL: "on" }, "TRACE_SQL", "malformed");
});

test("booleans accept 1/0 and yes/no alongside true/false (TRACE_SQL=1 stays valid)", async () => {
  const on = await loaded({ TRACE_SQL: "1", LOG_INCLUDE_BIND_VALUES: "yes" });
  assert.equal(on.logging.traceSql, true);
  assert.equal(on.logging.includeBindValues, true);
  const off = await loaded({ TRACE_SQL: "0", LOG_INCLUDE_BIND_VALUES: "no" });
  assert.equal(off.logging.traceSql, false);
  assert.equal(off.logging.includeBindValues, false);
  const words = await loaded({ TRACE_SQL: "true", LOG_INCLUDE_BIND_VALUES: "false" });
  assert.equal(words.logging.traceSql, true);
  assert.equal(words.logging.includeBindValues, false);
});

test("LOG_DIR and EVENTLOG_DUCKDB resolve absolute; EVENTLOG_DUCKDB defaults under DATA_DIR", async () => {
  const config = await loaded({ DATA_DIR: "./custom-data", LOG_DIR: "./custom-logs" });
  assert.equal(config.logging.dir, resolve("./custom-logs"));
  assert.equal(config.eventLog.duckdbPath, resolve("./custom-data", "eventlog.duckdb"));
});

test("an explicit EVENTLOG_DUCKDB overrides the DATA_DIR-relative default", async () => {
  const config = await loaded({ DATA_DIR: "./custom-data", EVENTLOG_DUCKDB: "./elsewhere/events.duckdb" });
  assert.equal(config.eventLog.duckdbPath, resolve("./elsewhere/events.duckdb"));
});

test("ConfigTest() with no overrides matches ConfigLive's documented defaults", async () => {
  const live = await loaded({});
  const fixture = await Effect.runPromise(Effect.provide(Config, ConfigTest()));
  assert.deepEqual(fixture, live);
});

test("ConfigTest(overrides) merges over defaults and ignores the environment", async () => {
  await withEnv({ LOG_LEVEL: "error", LOG_DIR: "/somewhere/else" }, async () => {
    const config = await Effect.runPromise(
      Effect.provide(
        Config,
        ConfigTest({ logging: { dir: "/tmp/logs" }, eventLog: { duckdbPath: "/tmp/e.duckdb" } }),
      ),
    );
    assert.equal(config.logging.dir, "/tmp/logs");
    assert.equal(config.logging.level, "info");
    assert.equal(config.eventLog.duckdbPath, "/tmp/e.duckdb");
  });
});

test("every option is declared exactly once, by env var, and renders one table row", () => {
  assert.equal(new Set(ENV_VARS).size, ENV_VARS.length);
  const rows = renderOptionsTable().split("\n").slice(2);
  assert.equal(rows.length, OPTIONS.length);
  for (const spec of OPTIONS) assert.ok(rows.some((row) => row.includes(`\`${spec.envVar}\``)), spec.envVar);
});
