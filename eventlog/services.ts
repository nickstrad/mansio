/**
 * Service tags and interface types for the event-log CLI (`scripts/logs.ts`). **Declarations only: no
 * implementations and no layers live here.**
 *
 * These are what was left of the Deno platform's `services.ts` after IR-6: the configuration of
 * the log tree and the DuckDB event log, the `EventSink` the CLI writes its own events through, and the
 * ingest and analysis services. The event log is written by the Go agent (`server/internal/eventlog`,
 * JSONL in the format of `core/events.ts`) and read here.
 */
import { Context, type Effect } from "effect";
import type { AnalysisError, IngestError, UnknownAnalysisQuery } from "../core/errors.ts";
import type { EventRecord, Level as EventLevel, Role } from "../core/events.ts";

// ===========================================================================
// Event types owned by core/events.ts, re-exported
// ===========================================================================

/**
 * The event vocabulary lives in `core/events.ts`; these are re-exported so `EventSink` and its users keep
 * one name for each. `EventRecord` is the canonical discriminated union over `kind`, `Role` the process
 * that writes a record (each gets its own `logs/<role>/` tree), and `EventLevel` is core's `Level`.
 */
export type { EventLevel, EventRecord, Role };

/** One ingest run's outcome. */
export interface IngestReport {
  readonly filesScanned: number;
  readonly filesIngested: number;
  readonly bytesIngested: number;
  readonly linesIngested: number;
  readonly linesRejected: number;
  readonly durationMs: number;
}

/** One analysis result row. */
export type Row = Readonly<Record<string, unknown>>;

// ===========================================================================
// Config (`ConfigLive`, `ConfigTest` in `eventlog/config.ts`)
// ===========================================================================

export interface LoggingConfig {
  /** `LOG_DIR`. Root of the `logs/<role>/<date>/` tree. */
  readonly dir: string;
  readonly level: EventLevel;
  readonly retentionDays: number;
  readonly gzipAfterDays: number;
  readonly maxTotalBytes: number;
  readonly maxFileBytes: number;
  /** `LOG_INCLUDE_BIND_VALUES`. Off by default: bind values are application data. */
  readonly includeBindValues: boolean;
  /** `TRACE_SQL`. Opt-in SQL tracing, as an event level. */
  readonly traceSql: boolean;
  readonly eventQueueCapacity: number;
  readonly eventFlushMs: number;
}

export interface EventLogConfig {
  /** `EVENTLOG_DUCKDB`. The persistent DuckDB event-log file. */
  readonly duckdbPath: string;
}

/**
 * The whole configuration, read once at layer construction. `eventlog/config.ts` is the only module
 * permitted to read the process environment.
 */
export interface ConfigService {
  readonly logging: LoggingConfig;
  readonly eventLog: EventLogConfig;
}

/** Live: `ConfigLive` (env). Test: `ConfigTest(overrides)`. */
export class Config extends Context.Tag("mansio/Config")<Config, ConfigService>() {}

// ===========================================================================
// EventSink — `EventSinkLive` (`eventlog/logger.ts`), `EventSinkTest` (tests/layers)
// ===========================================================================

export interface EventSinkService {
  /**
   * Record one event. **Cannot fail** — a logging problem must degrade (drop + count) rather than
   * surface in a caller's error channel. Dropped counts are themselves emitted as a `queue` event, so
   * loss is visible.
   */
  emit(event: EventRecord): Effect.Effect<void>;
}

/** Live: `EventSinkLive` (bounded queue → rotating JSONL). Test: `EventSinkTest` (`Ref<EventRecord[]>`). */
export class EventSink extends Context.Tag("mansio/EventSink")<EventSink, EventSinkService>() {}

// ===========================================================================
// EventLogIngest — `eventlog/ingest.ts`
// ===========================================================================

export interface EventLogIngestService {
  /** Idempotent: a second run over an unchanged log tree inserts nothing. */
  readonly ingest: Effect.Effect<IngestReport, IngestError>;
}

/** Live: `EventLogIngestLive` (DuckDB). */
export class EventLogIngest extends Context.Tag("mansio/EventLogIngest")<
  EventLogIngest,
  EventLogIngestService
>() {}

// ===========================================================================
// Analysis — `eventlog/catalog.ts` (`AnalysisLive`)
// ===========================================================================

export interface AnalysisService {
  /** Run a catalog query by name. Callers never supply SQL. */
  run(
    name: string,
    params: Readonly<Record<string, unknown>>,
  ): Effect.Effect<readonly Row[], AnalysisError | UnknownAnalysisQuery>;
  /** The catalog's names, for the error message `logs query` prints on a miss. */
  readonly names: Effect.Effect<readonly string[]>;
}

/** Live: `AnalysisLive` (over the event-log DuckDB file). No test double. */
export class Analysis extends Context.Tag("mansio/Analysis")<Analysis, AnalysisService>() {}
