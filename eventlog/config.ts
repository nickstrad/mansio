/**
 * The event-log CLI's env boundary (`scripts/logs.ts`). `ConfigLive` reads `Deno.env.toObject()` once at
 * layer construction — not at first use — decodes it against `OPTIONS` below, and fails with a typed
 * `ConfigError` rather than letting a bad value surface later as a confusing runtime failure.
 * `ConfigTest` ignores the environment entirely and merges caller overrides over the same table's
 * documented defaults.
 *
 * `OPTIONS` is the one place every option is declared: name, env var, type, default, description.
 * Adding an option is adding one row here plus wiring its decoded value into `assemble`'s return
 * object — the wiring keys are type-checked against the table, so a typo'd key is a compile error.
 *
 * These are the options the Deno platform's `config.ts` had for its log tree and event log
 * (IR-6 removed the rest with the platform). The log tree's writer is now agentd (`event_log` in its
 * config); the logging options here govern the CLI's own `reader` log and the retention sweep.
 */
import { resolve } from "node:path";
import { Effect, Layer } from "effect";
import * as Schema from "effect/Schema";
import { ConfigError } from "../core/errors.ts";
import {
  Config,
  type ConfigService,
  type EventLevel,
  type EventLogConfig,
  type LoggingConfig,
} from "./services.ts";

const EVENT_LEVELS = ["debug", "info", "warn", "error"] as const;

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
  /** Human-readable default, as documented. */
  readonly defaultLabel: string;
  readonly description: string;
}

/** The options table, in `ConfigService` order. */
export const OPTIONS = [
  {
    name: "dataDir",
    envVar: "DATA_DIR",
    type: "string",
    default: "./data",
    defaultLabel: "./data",
    description: "Directory the default EVENTLOG_DUCKDB lives in, resolved to an absolute path.",
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
    description: "Include SQL bind values in sql events. Off by default: bind values are application data. " +
      "Accepts true/false, 1/0, yes/no.",
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

  // --- event log ---
  {
    name: "eventlogDuckdbPath",
    envVar: "EVENTLOG_DUCKDB",
    type: "string",
    default: "",
    defaultLabel: "<DATA_DIR>/eventlog.duckdb",
    description: "Persistent DuckDB file backing the ingested event log, resolved to an absolute path. " +
      "Defaults to eventlog.duckdb inside DATA_DIR.",
  },
] as const;

/** Renders `OPTIONS` as a markdown table (for the docs). */
export function renderOptionsTable(): string {
  const header = "| Option | Env var | Type | Default | Description |\n| --- | --- | --- | --- | --- |";
  const rows = OPTIONS.map((spec) => {
    const type = spec.type === "enum" ? `enum(${spec.enumValues!.join("\\|")})` : spec.type;
    const name = `\`${spec.name}\``, env = `\`${spec.envVar}\``, fallback = `\`${spec.defaultLabel}\``;
    return `| ${name} | ${env} | ${type} | ${fallback} | ${spec.description} |`;
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
function num(values: Decoded, name: OptionName): number {
  return values[name] as number;
}
function bool(values: Decoded, name: OptionName): boolean {
  return values[name] as boolean;
}

/** Per-option decode has already run; this resolves the paths and assembles `ConfigService`. */
function assemble(values: Decoded): Effect.Effect<ConfigService, ConfigError> {
  const dataDir = resolve(str(values, "dataDir"));
  const eventlogDuckdbRaw = str(values, "eventlogDuckdbPath");
  const eventlogDuckdbPath = eventlogDuckdbRaw === ""
    ? resolve(dataDir, "eventlog.duckdb")
    : resolve(eventlogDuckdbRaw);
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
  const eventLog: EventLogConfig = { duckdbPath: eventlogDuckdbPath };
  return Effect.succeed({ logging, eventLog });
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

/** Every option's documented default, as a full `ConfigService`: computed once, ignoring the environment. */
const DEFAULT_CONFIG: ConfigService = Effect.runSync(buildConfig({}));

export interface ConfigOverrides {
  readonly logging?: Partial<LoggingConfig>;
  readonly eventLog?: Partial<EventLogConfig>;
}

/** Test: ignores the environment; merges `overrides` over the documented defaults. */
export function ConfigTest(overrides: ConfigOverrides = {}): Layer.Layer<Config> {
  const value: ConfigService = {
    logging: { ...DEFAULT_CONFIG.logging, ...overrides.logging },
    eventLog: { ...DEFAULT_CONFIG.eventLog, ...overrides.eventLog },
  };
  return Layer.succeed(Config, value);
}
