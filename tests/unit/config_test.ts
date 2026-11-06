import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";
import { Effect } from "effect";
import { Config } from "../../platform/services.ts";
import { ConfigLive, ConfigTest, OPTIONS, renderOptionsTable } from "../../platform/config.ts";

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

test("defaults: every option's documented default produces the matching ConfigService", async () => {
  await withEnv({}, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    const config = exit.value;
    assert.deepEqual(config.server, {
      hostname: "127.0.0.1",
      port: 8000,
      maxRequestBytes: 65_536,
      adminToken: "dev-only",
      dataDir: resolve("./data"),
    });
    assert.deepEqual(config.apps, [
      { id: "tic-tac-toe", token: "dev-only", storage: "columns" },
      { id: "tic-tac-toe-json", token: "dev-only", storage: "json" },
    ]);
    assert.deepEqual(config.backup, { intervalMs: 0 });
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
    assert.deepEqual(config.objectStore, {
      s3BaseUrl: undefined,
      region: "us-east-1",
      accessKeyId: undefined,
      secretAccessKey: undefined,
      sessionToken: undefined,
      localDir: resolve("./object-store"),
    });
    assert.deepEqual(config.eventLog, { duckdbPath: resolve("./data", "eventlog.duckdb") });
    // MT-02
    assert.deepEqual(config.tenancy, {
      mode: "off",
      databaseUrl: "postgres://mansio:mansio@localhost:5432/mansio",
      tenantsDir: resolve("./data", "tenants"),
      router: "local",
      betterAuthSecret: undefined,
      betterAuthUrl: "http://localhost:3000",
    });
  });
});

test("empty-string env values are treated as unset, same as absent", async () => {
  await withEnv({ PORT: "", LOG_LEVEL: "" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.server.port, 8000);
    assert.equal(exit.value.logging.level, "info");
  });
});

test("BACKUP_INTERVAL_MS=500 fails with ConfigError(out-of-range)", async () => {
  await withEnv({ BACKUP_INTERVAL_MS: "500" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Failure");
    if (exit._tag !== "Failure") return;
    const error = exit.cause._tag === "Fail" ? exit.cause.error : undefined;
    assert.equal(error?._tag, "ConfigError");
    assert.equal(error?.key, "BACKUP_INTERVAL_MS");
    assert.equal(error?.reason, "out-of-range");
  });
});

test("BACKUP_INTERVAL_MS=1000 (the boundary) succeeds", async () => {
  await withEnv({ BACKUP_INTERVAL_MS: "1000" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.backup.intervalMs, 1000);
  });
});

test("BACKUP_INTERVAL_MS=0 (disabled) succeeds", async () => {
  await withEnv({ BACKUP_INTERVAL_MS: "0" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
  });
});

test("BACKUP_INTERVAL_MS=not-a-number fails with ConfigError(malformed)", async () => {
  await withEnv({ BACKUP_INTERVAL_MS: "soon" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Failure");
    if (exit._tag !== "Failure") return;
    const error = exit.cause._tag === "Fail" ? exit.cause.error : undefined;
    assert.equal(error?._tag, "ConfigError");
    assert.equal(error?.key, "BACKUP_INTERVAL_MS");
    assert.equal(error?.reason, "malformed");
  });
});

test("S3_BASE_URL set without credentials fails with ConfigError(inconsistent)", async () => {
  await withEnv({ S3_BASE_URL: "https://bucket.example.com" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Failure");
    if (exit._tag !== "Failure") return;
    const error = exit.cause._tag === "Fail" ? exit.cause.error : undefined;
    assert.equal(error?._tag, "ConfigError");
    assert.equal(error?.key, "S3_BASE_URL");
    assert.equal(error?.reason, "inconsistent");
  });
});

test("S3_BASE_URL set with both credentials succeeds", async () => {
  await withEnv(
    {
      S3_BASE_URL: "https://bucket.example.com",
      AWS_ACCESS_KEY_ID: "AKIA...",
      AWS_SECRET_ACCESS_KEY: "secret",
    },
    async () => {
      const exit = await load();
      assert.equal(exit._tag, "Success");
      if (exit._tag !== "Success") return;
      assert.equal(exit.value.objectStore.s3BaseUrl, "https://bucket.example.com");
      assert.equal(exit.value.objectStore.accessKeyId, "AKIA...");
      assert.equal(exit.value.objectStore.secretAccessKey, "secret");
    },
  );
});

test("a non-numeric PORT fails with ConfigError(malformed)", async () => {
  await withEnv({ PORT: "eighty" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Failure");
    if (exit._tag !== "Failure") return;
    const error = exit.cause._tag === "Fail" ? exit.cause.error : undefined;
    assert.equal(error?._tag, "ConfigError");
    assert.equal(error?.key, "PORT");
    assert.equal(error?.reason, "malformed");
  });
});

test("an unrecognized LOG_LEVEL fails with ConfigError(malformed)", async () => {
  await withEnv({ LOG_LEVEL: "verbose" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Failure");
    if (exit._tag !== "Failure") return;
    const error = exit.cause._tag === "Fail" ? exit.cause.error : undefined;
    assert.equal(error?._tag, "ConfigError");
    assert.equal(error?.key, "LOG_LEVEL");
    assert.equal(error?.reason, "malformed");
  });
});

test("a LOG_INCLUDE_BIND_VALUES outside true/false/1/0/yes/no fails with ConfigError(malformed)", async () => {
  await withEnv({ LOG_INCLUDE_BIND_VALUES: "maybe" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Failure");
    if (exit._tag !== "Failure") return;
    const error = exit.cause._tag === "Fail" ? exit.cause.error : undefined;
    assert.equal(error?._tag, "ConfigError");
    assert.equal(error?.key, "LOG_INCLUDE_BIND_VALUES");
    assert.equal(error?.reason, "malformed");
  });
});

test("booleans accept 1/0 and yes/no alongside true/false (TRACE_SQL=1 stays valid)", async () => {
  await withEnv({ TRACE_SQL: "1", LOG_INCLUDE_BIND_VALUES: "yes" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.logging.traceSql, true);
    assert.equal(exit.value.logging.includeBindValues, true);
  });
  await withEnv({ TRACE_SQL: "0", LOG_INCLUDE_BIND_VALUES: "no" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.logging.traceSql, false);
    assert.equal(exit.value.logging.includeBindValues, false);
  });
});

test("DATA_DIR and EVENTLOG_DUCKDB are resolved to absolute paths; EVENTLOG_DUCKDB defaults under DATA_DIR", async () => {
  await withEnv({ DATA_DIR: "./custom-data" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.server.dataDir, resolve("./custom-data"));
    assert.equal(exit.value.eventLog.duckdbPath, resolve("./custom-data", "eventlog.duckdb"));
  });
});

test("an explicit EVENTLOG_DUCKDB overrides the DATA_DIR-relative default", async () => {
  await withEnv({ EVENTLOG_DUCKDB: "./elsewhere/events.duckdb" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.eventLog.duckdbPath, resolve("./elsewhere/events.duckdb"));
  });
});

test("ConfigTest() with no overrides matches ConfigLive's documented defaults", async () => {
  await withEnv({}, async () => {
    const liveExit = await load();
    assert.equal(liveExit._tag, "Success");
    if (liveExit._tag !== "Success") return;
    const testConfig = await Effect.runPromise(Effect.provide(Config, ConfigTest()));
    assert.deepEqual(testConfig, liveExit.value);
  });
});

test("ConfigTest(overrides) merges over defaults and ignores the environment", async () => {
  await withEnv({ PORT: "9999", BACKUP_INTERVAL_MS: "500" }, async () => {
    const config = await Effect.runPromise(
      Effect.provide(Config, ConfigTest({ server: { port: 1234 } })),
    );
    assert.equal(config.server.port, 1234);
    // Untouched fields keep the documented default, and the (otherwise-invalid) env is ignored.
    assert.equal(config.server.hostname, "127.0.0.1");
    assert.equal(config.backup.intervalMs, 0);
  });
});

test("every option is declared exactly once, by env var", () => {
  const envVars = OPTIONS.map((spec) => spec.envVar);
  assert.equal(new Set(envVars).size, envVars.length);
});

test("README 'Configuration' section matches the options table exactly", () => {
  const readmePath = fileURLToPath(new URL("../../README.md", import.meta.url));
  const readme = readFileSync(readmePath, "utf-8");
  // Anchor to the section: a row appended to (or left behind in) the table must fail too,
  // not just edits and deletions. Normalize whitespace so `deno fmt` on README.md stays green.
  // `deno fmt` also pads markdown table separator cells with dashes to the column width
  // (`| ------ |` instead of `| --- |`); canonicalize separator rows so the padded and
  // unpadded forms compare equal while content rows still compare exactly.
  const sectionStart = readme.indexOf("## Configuration");
  assert.ok(sectionStart !== -1, "README.md must contain a ## Configuration section");
  const nextSection = readme.indexOf("\n## ", sectionStart + 1);
  const section = readme.slice(sectionStart, nextSection === -1 ? undefined : nextSection);
  const norm = (s: string) =>
    s.split("\n").map((line) => {
      const collapsed = line.trim().replace(/\s+/g, " ");
      return /^\|(?:\s*:?-+:?\s*\|)+$/.test(collapsed) ? collapsed.replace(/-+/g, "-") : collapsed;
    });
  const readmeTable = section.split("\n").filter((line) => line.startsWith("|")).join("\n");
  assert.deepEqual(
    norm(readmeTable),
    norm(renderOptionsTable()),
    "README.md's Configuration table must match platform/config.ts's OPTIONS exactly (name, env var, type, default, description)",
  );
});

// --- MT-02: tenancy options ---

async function expectConfigError(
  vars: Readonly<Record<string, string | undefined>>,
  key: string,
  reason: string,
) {
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

test("TENANCY=bogus fails with ConfigError(malformed)", async () => {
  await expectConfigError({ TENANCY: "bogus" }, "TENANCY", "malformed");
});

test("TENANCY=better-auth without BETTER_AUTH_SECRET fails with ConfigError(inconsistent)", async () => {
  await expectConfigError({ TENANCY: "better-auth" }, "TENANCY", "inconsistent");
});

test("TENANCY=better-auth with BETTER_AUTH_SECRET succeeds", async () => {
  await withEnv({ TENANCY: "better-auth", BETTER_AUTH_SECRET: "s3cr3t" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.tenancy.mode, "better-auth");
    assert.equal(exit.value.tenancy.betterAuthSecret, "s3cr3t");
  });
});

test("TENANCY=api-key does not require BETTER_AUTH_SECRET", async () => {
  await withEnv({ TENANCY: "api-key" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.tenancy.mode, "api-key");
  });
});

test("tenantsDir defaults inside DATA_DIR", async () => {
  await withEnv({ DATA_DIR: "./custom-data" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.tenancy.tenantsDir, resolve("./custom-data", "tenants"));
  });
});

test("an explicit TENANTS_DIR is resolved to an absolute path", async () => {
  await withEnv({ TENANTS_DIR: "./elsewhere/tenants" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.tenancy.tenantsDir, resolve("./elsewhere/tenants"));
  });
});

test("TENANT_ROUTER=bogus fails with ConfigError(malformed)", async () => {
  await expectConfigError({ TENANT_ROUTER: "bogus" }, "TENANT_ROUTER", "malformed");
});

test("TENANT_ROUTER=control-plane succeeds", async () => {
  await withEnv({ TENANT_ROUTER: "control-plane" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.tenancy.router, "control-plane");
  });
});

// --- FFI-02: SQLite FFI options ---

test("sqlite defaults: library unset, statementDeadlineMs 5000, statementInterruptGraceMs 1000", async () => {
  await withEnv({}, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.deepEqual(exit.value.sqlite, {
      library: undefined,
      statementDeadlineMs: 5000,
      statementInterruptGraceMs: 1000,
    });
  });
});

test("SQLITE_LIBRARY decodes to the given path", async () => {
  await withEnv({ SQLITE_LIBRARY: "/x/libsqlite3.so.0" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.sqlite.library, "/x/libsqlite3.so.0");
  });
});

test("STATEMENT_DEADLINE_MS=0 fails with ConfigError(out-of-range)", async () => {
  await expectConfigError({ STATEMENT_DEADLINE_MS: "0" }, "STATEMENT_DEADLINE_MS", "out-of-range");
});

test("STATEMENT_DEADLINE_MS negative fails with ConfigError(out-of-range)", async () => {
  await expectConfigError({ STATEMENT_DEADLINE_MS: "-1" }, "STATEMENT_DEADLINE_MS", "out-of-range");
});

test("STATEMENT_DEADLINE_MS=NaN fails with ConfigError(out-of-range)", async () => {
  await expectConfigError({ STATEMENT_DEADLINE_MS: "NaN" }, "STATEMENT_DEADLINE_MS", "out-of-range");
});

test("STATEMENT_DEADLINE_MS=Infinity fails with ConfigError(out-of-range)", async () => {
  await expectConfigError({ STATEMENT_DEADLINE_MS: "Infinity" }, "STATEMENT_DEADLINE_MS", "out-of-range");
});

test("STATEMENT_INTERRUPT_GRACE_MS=Infinity fails with ConfigError(out-of-range)", async () => {
  await expectConfigError(
    { STATEMENT_INTERRUPT_GRACE_MS: "Infinity" },
    "STATEMENT_INTERRUPT_GRACE_MS",
    "out-of-range",
  );
});

test("STATEMENT_DEADLINE_MS non-numeric fails with ConfigError(malformed)", async () => {
  await expectConfigError({ STATEMENT_DEADLINE_MS: "soon" }, "STATEMENT_DEADLINE_MS", "malformed");
});

test("STATEMENT_DEADLINE_MS >= READER_CALL_TIMEOUT_MS fails with ConfigError(inconsistent)", async () => {
  await expectConfigError(
    { STATEMENT_DEADLINE_MS: "30000", READER_CALL_TIMEOUT_MS: "30000" },
    "STATEMENT_DEADLINE_MS",
    "inconsistent",
  );
  await expectConfigError(
    { STATEMENT_DEADLINE_MS: "40000" },
    "STATEMENT_DEADLINE_MS",
    "inconsistent",
  );
});

test("STATEMENT_DEADLINE_MS just under READER_CALL_TIMEOUT_MS succeeds", async () => {
  await withEnv({ STATEMENT_DEADLINE_MS: "29999", READER_CALL_TIMEOUT_MS: "30000" }, async () => {
    const exit = await load();
    assert.equal(exit._tag, "Success");
    if (exit._tag !== "Success") return;
    assert.equal(exit.value.sqlite.statementDeadlineMs, 29999);
  });
});

test("STATEMENT_INTERRUPT_GRACE_MS=0 fails with ConfigError(out-of-range)", async () => {
  await expectConfigError(
    { STATEMENT_INTERRUPT_GRACE_MS: "0" },
    "STATEMENT_INTERRUPT_GRACE_MS",
    "out-of-range",
  );
});
