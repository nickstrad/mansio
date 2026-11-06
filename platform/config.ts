/**
 * The single env boundary (plan item 08). `ConfigLive` reads `Deno.env.toObject()` once at layer
 * construction — not at first use — decodes it against `OPTIONS` below, and fails with a typed
 * `ConfigError` rather than letting a bad value surface later as a confusing runtime failure.
 * `ConfigTest` ignores the environment entirely and merges caller overrides over the same table's
 * documented defaults.
 *
 * `OPTIONS` is the one place every option is declared: name, env var, type, default, description.
 * Adding an option is adding one row here plus wiring its decoded value into `assemble`'s return
 * object — the wiring keys are type-checked against the table, so a typo'd key is a compile
 * error. `tests/unit/config_test.ts` checks the README "Configuration"
 * section against this table mechanically (`renderOptionsTable`), so the two cannot drift silently.
 *
 * Effect Schema (`effect/Schema`) enforces every per-option rule used here (number/boolean/enum
 * parsing); no rule in this module needed the Zod fallback the plan allows.
 */
import { resolve } from "node:path";
import { Effect, Layer } from "effect";
import * as Schema from "effect/Schema";
import { ConfigError } from "../core/errors.ts";
import {
  type AppConfig,
  type BackupConfig,
  Config,
  type ConfigService,
  type EventLevel,
  type EventLogConfig,
  type LoggingConfig,
  type ObjectStoreConfig,
  type ReaderConfig,
  type ServerConfig,
  type SqliteConfig,
  type TenancyConfig,
} from "./services.ts";

const EVENT_LEVELS = ["debug", "info", "warn", "error"] as const;
const TENANCY_MODES = ["off", "api-key", "better-auth"] as const;
const ROUTER_KINDS = ["local", "control-plane"] as const;
const DEV_TOKEN_DEFAULT = "dev-only";

/**
 * Boolean decoder: "true"/"false" are primary, but "1"/"0" and "yes"/"no" are accepted
 * too — `TRACE_SQL=1` is the long-standing documented spelling (README/SKIM), so rejecting
 * it would contradict the docs. Empty string never reaches here (`readOption` treats it
 * as unset).
 */
const BooleanFromLenientString = Schema.Literal("true", "false", "1", "0", "yes", "no").pipe(
  Schema.transform(Schema.Boolean, {
    strict: true,
    decode: (s) => s === "true" || s === "1" || s === "yes",
    encode: (b) => (b ? "true" : "false"),
  }),
);

type OptionType = "string" | "optional-string" | "number" | "boolean" | "enum";

export interface OptionSpec {
  /** The key `assemble` reads this option's decoded value under. */
  readonly name: string;
  readonly envVar: string;
  readonly type: OptionType;
  /** Only present for `type: "enum"`. */
  readonly enumValues?: readonly string[];
  /**
   * Decode-time fallback used when the env var is unset or empty (empty string is treated as
   * unset throughout this module). `""` is a sentinel only for `eventlogDuckdbPath`: its real
   * default is computed from the resolved `dataDir` in `assemble`, documented via `defaultLabel`.
   */
  readonly default: string | number | boolean | undefined;
  /** Human-readable default, exactly as documented in the README "Configuration" table. */
  readonly defaultLabel: string;
  readonly description: string;
}

/**
 * The options table. Order groups options by the `ConfigService` section they feed, matching
 * `platform/services.ts`'s field order within each section.
 */
export const OPTIONS = [
  // --- server ---
  {
    name: "port",
    envVar: "PORT",
    type: "number",
    default: 8000,
    defaultLabel: "8000",
    description: "HTTP server port.",
  },
  {
    name: "bindHost",
    envVar: "BIND_HOST",
    type: "string",
    default: "127.0.0.1",
    defaultLabel: "127.0.0.1",
    description: "HTTP server bind address.",
  },
  {
    name: "maxRequestBytes",
    envVar: "MAX_REQUEST_BYTES",
    type: "number",
    default: 65_536,
    defaultLabel: "65536",
    description: "Request body cap in bytes; larger bodies are rejected before parsing.",
  },
  {
    name: "dataDir",
    envVar: "DATA_DIR",
    type: "string",
    default: "./data",
    defaultLabel: "./data",
    description: "Root directory for per-app SQLite files, resolved to an absolute path.",
  },
  {
    name: "adminToken",
    envVar: "ADMIN_TOKEN",
    type: "string",
    default: DEV_TOKEN_DEFAULT,
    defaultLabel: DEV_TOKEN_DEFAULT,
    description: "Bearer token for the /admin/* routes.",
  },

  // --- apps ---
  {
    name: "columnAppToken",
    envVar: "COLUMN_APP_TOKEN",
    type: "string",
    default: DEV_TOKEN_DEFAULT,
    defaultLabel: DEV_TOKEN_DEFAULT,
    description: "Bearer token for the tic-tac-toe app (columns storage).",
  },
  {
    name: "jsonAppToken",
    envVar: "JSON_APP_TOKEN",
    type: "string",
    default: DEV_TOKEN_DEFAULT,
    defaultLabel: DEV_TOKEN_DEFAULT,
    description: "Bearer token for the tic-tac-toe-json app (json storage).",
  },

  // --- backup ---
  {
    name: "backupIntervalMs",
    envVar: "BACKUP_INTERVAL_MS",
    type: "number",
    default: 0,
    defaultLabel: "0",
    description:
      "Backup timer period in milliseconds. 0 disables the timer; otherwise must be finite and >= 1000.",
  },

  // --- logging ---
  {
    name: "logDir",
    envVar: "LOG_DIR",
    type: "string",
    default: "./logs",
    defaultLabel: "./logs",
    description: "Root of the logs/<role>/<date>/ tree.",
  },
  {
    name: "logLevel",
    envVar: "LOG_LEVEL",
    type: "enum",
    enumValues: EVENT_LEVELS,
    default: "info",
    defaultLabel: "info",
    description: "Minimum event level written to the log.",
  },
  {
    name: "logRetentionDays",
    envVar: "LOG_RETENTION_DAYS",
    type: "number",
    default: 30,
    defaultLabel: "30",
    description: "Days a log file is kept before deletion.",
  },
  {
    name: "logGzipAfterDays",
    envVar: "LOG_GZIP_AFTER_DAYS",
    type: "number",
    default: 7,
    defaultLabel: "7",
    description: "Days a log file is kept uncompressed before gzip.",
  },
  {
    name: "logMaxTotalBytes",
    envVar: "LOG_MAX_TOTAL_BYTES",
    type: "number",
    default: 1_073_741_824,
    defaultLabel: "1073741824",
    description: "Total bytes the log tree may occupy before the oldest files are swept.",
  },
  {
    name: "logMaxFileBytes",
    envVar: "LOG_MAX_FILE_BYTES",
    type: "number",
    default: 67_108_864,
    defaultLabel: "67108864",
    description: "Bytes a single log file may reach before it rolls over.",
  },
  {
    name: "logIncludeBindValues",
    envVar: "LOG_INCLUDE_BIND_VALUES",
    type: "boolean",
    default: false,
    defaultLabel: "false",
    description:
      "Include SQL bind values in sql events. Off by default: bind values are application data. Accepts true/false, 1/0, yes/no.",
  },
  {
    name: "traceSql",
    envVar: "TRACE_SQL",
    type: "boolean",
    default: false,
    defaultLabel: "false",
    description: "Emit an event for every compiled SQL statement. Accepts true/false, 1/0, yes/no.",
  },
  {
    name: "eventQueueCapacity",
    envVar: "EVENT_QUEUE_CAPACITY",
    type: "number",
    default: 10_000,
    defaultLabel: "10000",
    description:
      "Depth of the in-process dropping queue that buffers events before they are flushed to disk.",
  },
  {
    name: "eventFlushMs",
    envVar: "EVENT_FLUSH_MS",
    type: "number",
    default: 250,
    defaultLabel: "250",
    description: "Interval in milliseconds between batched event-queue flushes.",
  },

  // --- object store ---
  {
    name: "s3BaseUrl",
    envVar: "S3_BASE_URL",
    type: "optional-string",
    default: undefined,
    defaultLabel: "(unset)",
    description:
      "Base URL of the S3 (or S3-compatible) bucket for backups. Absent selects the local object-store stand-in.",
  },
  {
    name: "awsRegion",
    envVar: "AWS_REGION",
    type: "string",
    default: "us-east-1",
    defaultLabel: "us-east-1",
    description: "AWS region used to sign S3 requests.",
  },
  {
    name: "awsAccessKeyId",
    envVar: "AWS_ACCESS_KEY_ID",
    type: "optional-string",
    default: undefined,
    defaultLabel: "(unset)",
    description: "AWS access key id. Required when S3_BASE_URL is set.",
  },
  {
    name: "awsSecretAccessKey",
    envVar: "AWS_SECRET_ACCESS_KEY",
    type: "optional-string",
    default: undefined,
    defaultLabel: "(unset)",
    description: "AWS secret access key. Required when S3_BASE_URL is set.",
  },
  {
    name: "awsSessionToken",
    envVar: "AWS_SESSION_TOKEN",
    type: "optional-string",
    default: undefined,
    defaultLabel: "(unset)",
    description: "AWS session token, for temporary credentials.",
  },
  {
    name: "localObjectDir",
    envVar: "LOCAL_OBJECT_DIR",
    type: "string",
    default: "./object-store",
    defaultLabel: "./object-store",
    description: "Local object-store directory used when S3_BASE_URL is unset, resolved to an absolute path.",
  },

  // --- event log ---
  {
    name: "eventlogDuckdbPath",
    envVar: "EVENTLOG_DUCKDB",
    type: "string",
    default: "",
    defaultLabel: "<DATA_DIR>/eventlog.duckdb",
    description:
      "Persistent DuckDB file backing the ingested event log, resolved to an absolute path. Defaults to eventlog.duckdb inside DATA_DIR.",
  },

  // --- reader ---
  {
    name: "readerCallTimeoutMs",
    envVar: "READER_CALL_TIMEOUT_MS",
    type: "number",
    default: 30_000,
    defaultLabel: "30000",
    description:
      "Per-call deadline in milliseconds for a request to the reader process; a call with no reply by then fails with HTTP 502. Must be finite and > 0.",
  },

  // --- sqlite ---
  {
    name: "sqliteLibrary",
    envVar: "SQLITE_LIBRARY",
    type: "optional-string",
    default: undefined,
    defaultLabel: "(unset)",
    description:
      "Path or soname of the system libsqlite3 loaded by the FFI binding; must be libsqlite3 >= 3.37.0. vm-1 has two copies: 3.45.1 under /usr/lib/x86_64-linux-gnu/libsqlite3.so.0 and 3.53.4 under /usr/local/lib. Unset, the binding's default list applies, which tries the distro path first.",
  },
  {
    name: "statementDeadlineMs",
    envVar: "STATEMENT_DEADLINE_MS",
    type: "number",
    default: 5000,
    defaultLabel: "5000",
    description:
      "Deadline in milliseconds after which an unbounded statement (any select that is not a primary-key lookup, `count`, `countBy`) is interrupted via sqlite3_interrupt and fails with a typed deadline error (HTTP 504 once FFI-05 lands). Must be finite, > 0 and < READER_CALL_TIMEOUT_MS.",
  },
  {
    name: "statementInterruptGraceMs",
    envVar: "STATEMENT_INTERRUPT_GRACE_MS",
    type: "number",
    default: 1000,
    defaultLabel: "1000",
    description:
      "Milliseconds to wait after the interrupt for the statement to return; if it has not, an error-level lifecycle event is emitted and the connection is poisoned. Must be finite and > 0.",
  },

  // --- tenancy ---
  {
    name: "tenancyMode",
    envVar: "TENANCY",
    type: "enum",
    enumValues: TENANCY_MODES,
    default: "off",
    defaultLabel: "off",
    description:
      "Multitenancy mode. `off` preserves the pre-tenancy single-database, app-token behavior; `api-key` and `better-auth` are reserved for later multitenancy items.",
  },
  {
    name: "databaseUrl",
    envVar: "DATABASE_URL",
    type: "string",
    default: "postgres://mansio:mansio@localhost:5432/mansio",
    defaultLabel: "postgres://mansio:mansio@localhost:5432/mansio",
    description:
      "Postgres connection URL for the multitenancy central database (tenant registry). Unused while tenancy mode is `off`.",
  },
  {
    name: "tenantsDir",
    envVar: "TENANTS_DIR",
    type: "string",
    default: "",
    defaultLabel: "<DATA_DIR>/tenants",
    description:
      "Root directory for per-tenant SQLite files, resolved to an absolute path. Defaults to tenants inside DATA_DIR.",
  },
  {
    name: "tenantRouter",
    envVar: "TENANT_ROUTER",
    type: "enum",
    enumValues: ROUTER_KINDS,
    default: "local",
    defaultLabel: "local",
    description:
      "Router kind selecting tenant storage locations. Only `local` exists today; `control-plane` is reserved for the future.",
  },
  {
    name: "betterAuthSecret",
    envVar: "BETTER_AUTH_SECRET",
    type: "optional-string",
    default: undefined,
    defaultLabel: "(unset)",
    description:
      "Secret used to verify Better Auth session tokens. No default; required when TENANCY is `better-auth`.",
  },
  {
    name: "betterAuthUrl",
    envVar: "BETTER_AUTH_URL",
    type: "string",
    default: "http://localhost:3000",
    defaultLabel: "http://localhost:3000",
    description: "Base URL of the Better Auth instance issuing session tokens.",
  },
] as const satisfies readonly OptionSpec[];

/** Renders `OPTIONS` as the exact markdown table the README "Configuration" section must contain. */
export function renderOptionsTable(): string {
  const header = "| Option | Env var | Type | Default | Description |\n| --- | --- | --- | --- | --- |";
  const rows = OPTIONS.map((spec) => {
    const type = spec.type === "enum" ? `enum(${spec.enumValues!.join("\\|")})` : spec.type;
    return `| \`${spec.name}\` | \`${spec.envVar}\` | ${type} | \`${spec.defaultLabel}\` | ${spec.description} |`;
  });
  return [header, ...rows].join("\n");
}

type EnvRecord = Readonly<Record<string, string | undefined>>;
/** The option names as a literal union, so a typo'd key in `assemble` is a compile
 * error instead of silently reading `undefined`. */
type OptionName = typeof OPTIONS[number]["name"];
type Decoded = Readonly<Record<OptionName, string | number | boolean | undefined>>;

function configError(envVar: string, reason: ConfigError["reason"], detail: string): ConfigError {
  return new ConfigError({ key: envVar, reason, detail });
}

function decodeScalar(
  spec: OptionSpec,
  raw: string,
): Effect.Effect<string | number | boolean, ConfigError> {
  switch (spec.type) {
    case "string":
    case "optional-string":
      return Effect.succeed(raw);
    case "number":
      return Effect.mapError(
        Schema.decodeUnknown(Schema.NumberFromString)(raw),
        () =>
          configError(
            spec.envVar,
            "malformed",
            `${spec.envVar} must be a number, got ${JSON.stringify(raw)}`,
          ),
      );
    case "boolean":
      return Effect.mapError(
        Schema.decodeUnknown(BooleanFromLenientString)(raw),
        () =>
          configError(
            spec.envVar,
            "malformed",
            `${spec.envVar} must be true/false, 1/0, or yes/no, got ${JSON.stringify(raw)}`,
          ),
      );
    case "enum":
      return Effect.mapError(
        Schema.decodeUnknown(Schema.Literal(...spec.enumValues!))(raw),
        () =>
          configError(
            spec.envVar,
            "malformed",
            `${spec.envVar} must be one of ${spec.enumValues!.join(", ")}, got ${JSON.stringify(raw)}`,
          ),
      );
  }
}

/** Empty string is treated as unset, same as an absent env var, throughout this module. */
function readOption(
  env: EnvRecord,
  spec: OptionSpec,
): Effect.Effect<string | number | boolean | undefined, ConfigError> {
  const raw = env[spec.envVar];
  if (raw === undefined || raw === "") return Effect.succeed(spec.default);
  return decodeScalar(spec, raw);
}

function decodeAll(env: EnvRecord): Effect.Effect<Decoded, ConfigError> {
  // Keyed by the literal option names so the decoded record matches `Decoded` exactly.
  const effects = {} as Record<
    OptionName,
    Effect.Effect<string | number | boolean | undefined, ConfigError>
  >;
  for (const spec of OPTIONS) effects[spec.name] = readOption(env, spec);
  return Effect.all(effects);
}

function str(values: Decoded, name: OptionName): string {
  return values[name] as string;
}
function optStr(values: Decoded, name: OptionName): string | undefined {
  return values[name] as string | undefined;
}
function num(values: Decoded, name: OptionName): number {
  return values[name] as number;
}
function bool(values: Decoded, name: OptionName): boolean {
  return values[name] as boolean;
}

/** Per-option decode has already run; this applies the cross-field rules and assembles `ConfigService`. */
function assemble(values: Decoded): Effect.Effect<ConfigService, ConfigError> {
  const backupIntervalMs = num(values, "backupIntervalMs");
  if (backupIntervalMs !== 0 && (!Number.isFinite(backupIntervalMs) || backupIntervalMs < 1000)) {
    return Effect.fail(
      configError(
        "BACKUP_INTERVAL_MS",
        "out-of-range",
        `BACKUP_INTERVAL_MS must be 0 or >= 1000 and finite, got ${backupIntervalMs}`,
      ),
    );
  }

  const readerCallTimeoutMs = num(values, "readerCallTimeoutMs");
  if (!Number.isFinite(readerCallTimeoutMs) || readerCallTimeoutMs <= 0) {
    return Effect.fail(
      configError(
        "READER_CALL_TIMEOUT_MS",
        "out-of-range",
        `READER_CALL_TIMEOUT_MS must be finite and > 0, got ${readerCallTimeoutMs}`,
      ),
    );
  }

  const s3BaseUrl = optStr(values, "s3BaseUrl");
  const awsAccessKeyId = optStr(values, "awsAccessKeyId");
  const awsSecretAccessKey = optStr(values, "awsSecretAccessKey");
  if (s3BaseUrl !== undefined && (awsAccessKeyId === undefined || awsSecretAccessKey === undefined)) {
    return Effect.fail(
      configError(
        "S3_BASE_URL",
        "inconsistent",
        "S3_BASE_URL is set but AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY is missing",
      ),
    );
  }

  const statementDeadlineMs = num(values, "statementDeadlineMs");
  if (!Number.isFinite(statementDeadlineMs) || statementDeadlineMs <= 0) {
    return Effect.fail(
      configError(
        "STATEMENT_DEADLINE_MS",
        "out-of-range",
        `STATEMENT_DEADLINE_MS must be finite and > 0, got ${statementDeadlineMs}`,
      ),
    );
  }
  if (statementDeadlineMs >= readerCallTimeoutMs) {
    return Effect.fail(
      configError(
        "STATEMENT_DEADLINE_MS",
        "inconsistent",
        `STATEMENT_DEADLINE_MS (${statementDeadlineMs}) must be < READER_CALL_TIMEOUT_MS (${readerCallTimeoutMs}) so the in-statement interrupt fires first`,
      ),
    );
  }

  const statementInterruptGraceMs = num(values, "statementInterruptGraceMs");
  if (!Number.isFinite(statementInterruptGraceMs) || statementInterruptGraceMs <= 0) {
    return Effect.fail(
      configError(
        "STATEMENT_INTERRUPT_GRACE_MS",
        "out-of-range",
        `STATEMENT_INTERRUPT_GRACE_MS must be finite and > 0, got ${statementInterruptGraceMs}`,
      ),
    );
  }

  const tenancyMode = str(values, "tenancyMode") as TenancyConfig["mode"];
  const betterAuthSecret = optStr(values, "betterAuthSecret");
  if (tenancyMode === "better-auth" && betterAuthSecret === undefined) {
    return Effect.fail(
      configError("TENANCY", "inconsistent", "TENANCY=better-auth requires BETTER_AUTH_SECRET"),
    );
  }

  const dataDir = resolve(str(values, "dataDir"));
  const eventlogDuckdbRaw = str(values, "eventlogDuckdbPath");
  const eventlogDuckdbPath = eventlogDuckdbRaw === ""
    ? resolve(dataDir, "eventlog.duckdb")
    : resolve(eventlogDuckdbRaw);

  const tenantsDirRaw = str(values, "tenantsDir");
  const tenantsDir = tenantsDirRaw === "" ? resolve(dataDir, "tenants") : resolve(tenantsDirRaw);

  const adminToken = str(values, "adminToken");
  const columnAppToken = str(values, "columnAppToken");
  const jsonAppToken = str(values, "jsonAppToken");
  // A dev-only token still in effect is reported by `platform/server.ts` at startup, as one
  // `lifecycle` warn event (phase `config`), not here: the event sink consumes Config, so emitting
  // from inside ConfigLive would be a layer cycle. The defaults themselves keep working either way.

  const server: ServerConfig = {
    hostname: str(values, "bindHost"),
    port: num(values, "port"),
    maxRequestBytes: num(values, "maxRequestBytes"),
    adminToken,
    dataDir,
  };
  const apps: readonly AppConfig[] = [
    { id: "tic-tac-toe", token: columnAppToken, storage: "columns" },
    { id: "tic-tac-toe-json", token: jsonAppToken, storage: "json" },
  ];
  const backup: BackupConfig = { intervalMs: backupIntervalMs };
  const logging: LoggingConfig = {
    dir: resolve(str(values, "logDir")),
    level: str(values, "logLevel") as EventLevel,
    retentionDays: num(values, "logRetentionDays"),
    gzipAfterDays: num(values, "logGzipAfterDays"),
    maxTotalBytes: num(values, "logMaxTotalBytes"),
    maxFileBytes: num(values, "logMaxFileBytes"),
    includeBindValues: bool(values, "logIncludeBindValues"),
    traceSql: bool(values, "traceSql"),
    eventQueueCapacity: num(values, "eventQueueCapacity"),
    eventFlushMs: num(values, "eventFlushMs"),
  };
  const objectStore: ObjectStoreConfig = {
    s3BaseUrl,
    region: str(values, "awsRegion"),
    accessKeyId: awsAccessKeyId,
    secretAccessKey: awsSecretAccessKey,
    sessionToken: optStr(values, "awsSessionToken"),
    localDir: resolve(str(values, "localObjectDir")),
  };
  const eventLog: EventLogConfig = { duckdbPath: eventlogDuckdbPath };
  const reader: ReaderConfig = { callTimeoutMs: readerCallTimeoutMs };
  const sqlite: SqliteConfig = {
    library: optStr(values, "sqliteLibrary"),
    statementDeadlineMs,
    statementInterruptGraceMs,
  };

  const tenancy: TenancyConfig = {
    mode: tenancyMode,
    databaseUrl: str(values, "databaseUrl"),
    tenantsDir,
    router: str(values, "tenantRouter") as TenancyConfig["router"],
    betterAuthSecret,
    betterAuthUrl: str(values, "betterAuthUrl"),
  };

  return Effect.succeed({ server, apps, backup, logging, objectStore, eventLog, reader, sqlite, tenancy });
}

function buildConfig(env: EnvRecord): Effect.Effect<ConfigService, ConfigError> {
  return Effect.flatMap(decodeAll(env), assemble);
}

/**
 * Live: reads `Deno.env.toObject()` once when this layer is constructed. No other `Deno.env` read
 * exists in this module.
 */
export const ConfigLive: Layer.Layer<Config, ConfigError> = Layer.effect(
  Config,
  Effect.flatMap(Effect.sync(() => Deno.env.toObject() as EnvRecord), buildConfig),
);

/** Every option's documented default, as a full `ConfigService` — computed once, ignoring the environment. */
const DEFAULT_CONFIG: ConfigService = Effect.runSync(buildConfig({}));

export interface ConfigOverrides {
  readonly server?: Partial<ServerConfig>;
  readonly apps?: readonly AppConfig[];
  readonly backup?: Partial<BackupConfig>;
  readonly logging?: Partial<LoggingConfig>;
  readonly objectStore?: Partial<ObjectStoreConfig>;
  readonly eventLog?: Partial<EventLogConfig>;
  readonly reader?: Partial<ReaderConfig>;
  readonly sqlite?: Partial<SqliteConfig>;
  readonly tenancy?: Partial<TenancyConfig>;
}

/** Test: ignores the environment; merges `overrides` over the documented defaults. */
export function ConfigTest(overrides: ConfigOverrides = {}): Layer.Layer<Config> {
  const value: ConfigService = {
    server: { ...DEFAULT_CONFIG.server, ...overrides.server },
    apps: overrides.apps ?? DEFAULT_CONFIG.apps,
    backup: { ...DEFAULT_CONFIG.backup, ...overrides.backup },
    logging: { ...DEFAULT_CONFIG.logging, ...overrides.logging },
    objectStore: { ...DEFAULT_CONFIG.objectStore, ...overrides.objectStore },
    eventLog: { ...DEFAULT_CONFIG.eventLog, ...overrides.eventLog },
    reader: { ...DEFAULT_CONFIG.reader, ...overrides.reader },
    sqlite: { ...DEFAULT_CONFIG.sqlite, ...overrides.sqlite },
    tenancy: { ...DEFAULT_CONFIG.tenancy, ...overrides.tenancy },
  };
  return Layer.succeed(Config, value);
}
