/**
 * Pure ingest planning for the JSONL → DuckDB event log (plan item 21): given one source file's
 * bytes and its `_ingest_state` row, decide what to read, which complete lines that yields, and what
 * the row becomes. `eventlog/ingest.ts` does the I/O around it; the property suite in
 * `tests/property/offsets_test.ts` drives the same functions through adversarial file histories.
 *
 * This module is PURE: no imports, no I/O. Hashing is injected (`PrefixHash`) because `core/` may
 * not import a crypto implementation.
 *
 * ## The offset model
 *
 * A source file is identified by its path relative to `LOG_DIR`. Its state row records
 * `bytesIngested` — always just past a `\n`, so it is a line boundary — and `prefixSha256`, the
 * SHA-256 of exactly bytes `[0, bytesIngested)`. On every run the planner re-hashes that prefix of
 * the file as it is now and compares:
 *
 * | file now                                   | decision                                                |
 * | ------------------------------------------ | ------------------------------------------------------- |
 * | no state row                               | `new`: read from 0                                      |
 * | size >= bytesIngested, prefix hash matches | `append`: read from `bytesIngested` (or `skip`)         |
 * | size < bytesIngested                       | `rewritten` (`truncated`): read from 0, flag for dedupe |
 * | prefix hash differs                        | `rewritten` (`prefix-mismatch`): likewise               |
 *
 * **An intact prefix is never read for ingestion again.** Only bytes past `bytesIngested` become
 * lines; the prefix is only hashed. That is what makes a second run over an unchanged tree insert
 * nothing: every file is `skip` (or ends in a partial line that is not consumed), and no row changes.
 *
 * **Only complete lines are consumed.** A read stops after the last `\n`; a trailing partial line
 * (a write in flight, or a crash mid-write) stays unconsumed until its terminator lands, so it is
 * never ingested half-written and never skipped.
 *
 * **A rewritten file is a new generation, never a loss.** The rows already inserted from it cannot
 * be removed (event tables are append-only), and the offset model cannot know which of the file's
 * current lines it has already seen. So a rewrite is read in full from byte 0 with every line
 * marked `dedupe`: the platform inserts only lines whose `eventId` is not already in
 * `platform_events`. Lines the platform cannot dedupe (malformed lines have no `eventId`) may be
 * quarantined a second time; quarantine is a diagnostic file, not an event table.
 *
 * **Once rewritten, always deduplicated.** The prefix hash vouches only for `[0, bytesIngested)`.
 * After a rewrite, the bytes past that offset can hold *old* lines: a truncation that tears off an
 * ingested line's `\n` leaves the line behind, unconsumed, until a later append restores the
 * terminator; and a second rewrite can bring back lines from an earlier generation past any offset
 * the row could record. So a detected rewrite sets the row's `dedupe` flag, and every line read from
 * that file afterwards — in this run and every later `append` — is deduplicated on `eventId`. The
 * flag is never cleared: the offset model can no longer vouch for that path. It costs one anti-join
 * per batch, and only for files something outside the platform rewrote; the platform's own writers
 * only append. (The plan's §4.1 `_ingest_state` had no such column; the property suite found the
 * double inserts without it — first a torn terminator, then a byte horizon defeated by a second
 * rewrite.)
 *
 * The threat model is the one the log writers actually present: append-only files written in
 * whole lines, plus truncation, rewrite, or delete-and-recreate by something outside the platform.
 * Out of model, and not deduplicated: a line *moved* into a file that was never rewritten (it is a
 * new line of that file), and an outside append that duplicates lines while keeping the ingested
 * prefix intact (`cat f f > f` reads as an ordinary append). Both are duplicate *production*, which
 * offsets cannot tell from new events.
 *
 * Cost: the prefix is re-hashed on every run, so a run reads every `.jsonl` file in full. Retention
 * gzips dated directories after `gzipAfterDays` (default 2), and `.gz` files are not ingested, so the
 * hashed set is bounded by a few days of logs. A windowed hash would be cheaper and would miss
 * truncate-and-regrow rewrites inside the window; exactness was chosen over speed.
 */

/** One `_ingest_state` row. */
export interface IngestStateRow {
  /** Path relative to `LOG_DIR`, `/`-separated: `server/2026-10-10/events-14-31887-001.jsonl`. */
  readonly sourceFile: string;
  /** Bytes of the file consumed so far. Always 0 or just past a `\n`. */
  readonly bytesIngested: number;
  /** Lowercase hex SHA-256 of bytes `[0, bytesIngested)`. */
  readonly prefixSha256: string;
  /** Every line read from this file is deduplicated on `eventId`. Set by a detected rewrite; sticky. */
  readonly dedupe: boolean;
  /** ISO-8601 instant of the run that last changed this row. */
  readonly lastIngestedAt: string;
}

/** Hex SHA-256 of a byte prefix. Injected: `core/` imports no crypto. */
export type PrefixHash = (prefix: Uint8Array) => string;

/** Why a file with a state row is no longer the file that row describes. */
export type RewriteReason = "truncated" | "prefix-mismatch";

/** What to do with one source file this run. */
export type Decision =
  /** Nothing past the recorded offset (or an empty new file). No read, no row change. */
  | { readonly kind: "skip"; readonly sourceFile: string }
  /** Read `[from, size)`; consume through its last `\n`. */
  | {
    readonly kind: "read";
    readonly sourceFile: string;
    readonly mode: "new" | "append" | "rewritten";
    readonly from: number;
    readonly size: number;
    /** Set iff `mode === "rewritten"`. */
    readonly rewrite?: RewriteReason;
    /** The row this file had before the run, when it had one. */
    readonly previous?: IngestStateRow;
    /** Insert only lines whose `eventId` is not already ingested: the file is or was rewritten. */
    readonly dedupe: boolean;
  };

/** One complete, non-blank line: `bytes.subarray(start, end)`, terminator and any `\r` excluded. */
export interface LineSpan {
  /** Byte offset of the line's first byte in the file; becomes `platform_events.source_offset`. */
  readonly start: number;
  readonly end: number;
}

/** Everything a run needs to know about one file. */
export interface FilePlan {
  readonly decision: Decision;
  /** Complete, non-blank lines in `[from, end)`, in file order. Empty for `skip`. */
  readonly lines: readonly LineSpan[];
  /** Consumed through here (exclusive). Equals `from` when no complete line was available. */
  readonly end: number;
  /** The row after this run commits, or `undefined` to leave the row exactly as it is. */
  readonly nextState?: IngestStateRow;
}

const NEWLINE = 0x0a;
const CARRIAGE_RETURN = 0x0d;

/**
 * How many leading bytes of the file must be hashed to judge `state`: its `bytesIngested`, or
 * `undefined` when there is no row or the file is already shorter than the row says (truncation
 * needs no hash).
 */
export function prefixToVerify(state: IngestStateRow | undefined, size: number): number | undefined {
  if (state === undefined || size < state.bytesIngested) return undefined;
  return state.bytesIngested;
}

/**
 * The decision for one file. `prefixSha256` must be the hash of the first
 * `prefixToVerify(state, size)` bytes of the file as it is now, and is ignored when that is
 * `undefined`. A missing hash where one is needed is treated as a mismatch: re-reading with dedupe
 * is safe, trusting an unverified prefix is not.
 *
 * A rewrite sets `dedupe`; an append inherits it from the row.
 */
export function decide(
  sourceFile: string,
  state: IngestStateRow | undefined,
  size: number,
  prefixSha256: string | undefined,
): Decision {
  if (state === undefined) {
    return size === 0
      ? { kind: "skip", sourceFile }
      : { kind: "read", sourceFile, mode: "new", from: 0, size, dedupe: false };
  }
  if (size < state.bytesIngested) {
    return rewritten(sourceFile, state, size, "truncated");
  }
  if (prefixSha256 !== state.prefixSha256) {
    return rewritten(sourceFile, state, size, "prefix-mismatch");
  }
  if (size === state.bytesIngested) return { kind: "skip", sourceFile };
  return {
    kind: "read",
    sourceFile,
    mode: "append",
    from: state.bytesIngested,
    size,
    previous: state,
    dedupe: state.dedupe,
  };
}

function rewritten(
  sourceFile: string,
  previous: IngestStateRow,
  size: number,
  rewrite: RewriteReason,
): Decision {
  return { kind: "read", sourceFile, mode: "rewritten", from: 0, size, rewrite, previous, dedupe: true };
}

/**
 * The complete lines in `bytes[from, bytes.length)`. Consumption stops just past the last `\n`;
 * anything after it is a partial line and is left for a later run. Blank lines (empty, or a lone
 * `\r`) are consumed but yield no span. One trailing `\r` is stripped from each line, so CRLF files
 * ingest the same as LF files.
 */
export function completeLines(bytes: Uint8Array, from: number): { lines: LineSpan[]; end: number } {
  const lines: LineSpan[] = [];
  let lineStart = from;
  let end = from;
  for (let i = from; i < bytes.length; i++) {
    if (bytes[i] !== NEWLINE) continue;
    let lineEnd = i;
    if (lineEnd > lineStart && bytes[lineEnd - 1] === CARRIAGE_RETURN) lineEnd -= 1;
    if (lineEnd > lineStart) lines.push({ start: lineStart, end: lineEnd });
    lineStart = i + 1;
    end = i + 1;
  }
  return { lines, end };
}

/**
 * Plans one file end to end: decide, cut complete lines, and compute the row a successful commit
 * leaves behind.
 *
 * - `skip`, or a `new`/`append` read that found no complete line: `nextState` is `undefined`, so
 *   the row (or its absence) is left untouched — an idle run changes nothing, not even
 *   `lastIngestedAt`.
 * - Any read that consumed bytes, and every `rewritten` read (even one that consumed nothing, so
 *   the row stops describing the old generation): `nextState` records `end`, the hash of
 *   `[0, end)`, and the decision's `dedupe` flag.
 */
export function planFile(
  sourceFile: string,
  state: IngestStateRow | undefined,
  bytes: Uint8Array,
  hash: PrefixHash,
  now: string,
): FilePlan {
  const verify = prefixToVerify(state, bytes.length);
  const decision = decide(
    sourceFile,
    state,
    bytes.length,
    verify === undefined ? undefined : hash(bytes.subarray(0, verify)),
  );
  if (decision.kind === "skip") {
    return { decision, lines: [], end: state?.bytesIngested ?? 0 };
  }
  const { lines, end } = completeLines(bytes, decision.from);
  if (end === decision.from && decision.mode !== "rewritten") {
    return { decision, lines, end };
  }
  return {
    decision,
    lines,
    end,
    nextState: {
      sourceFile,
      bytesIngested: end,
      prefixSha256: hash(bytes.subarray(0, end)),
      dedupe: decision.dedupe,
      lastIngestedAt: now,
    },
  };
}

// ---------------------------------------------------------------------------
// Inventory: which files are event-log sources, and where their rejects go
// ---------------------------------------------------------------------------

/** The roles whose trees are ingested. Mirrors `core/logging/rotation.ts`'s `LogRole`. */
export const SOURCE_ROLES = ["server", "reader", "restore", "cli"] as const;
export type SourceRole = typeof SOURCE_ROLES[number];

/** A source file's coordinates, parsed from its relative path. */
export interface SourceFile {
  readonly path: string;
  readonly role: SourceRole;
  readonly date: string;
  readonly hour: number;
  readonly pid: number;
  readonly seq: number;
}

/**
 * `<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl`, the layout `rotation.currentPath` writes under
 * `LOG_DIR`. `.jsonl.gz` files and `rejected.jsonl` deliberately do not match: gzipped files are
 * closed history that retention produced after ingest, and quarantine must never be re-ingested.
 */
const SOURCE_PATTERN =
  /^(server|reader|restore|cli)\/(\d{4}-\d{2}-\d{2})\/events-(\d{2})-(\d+)-(\d{3,})\.jsonl$/;

/** Parses a relative source path, or `undefined` if it is not an event-log source file. */
export function parseSourceFile(path: string): SourceFile | undefined {
  const match = SOURCE_PATTERN.exec(path);
  if (match === null) return undefined;
  return {
    path,
    role: match[1] as SourceRole,
    date: match[2],
    hour: Number(match[3]),
    pid: Number(match[4]),
    seq: Number(match[5]),
  };
}

/** Chronological-ish ingest order: date, hour, role, pid, seq. Total and deterministic. */
export function compareSourceFiles(a: SourceFile, b: SourceFile): number {
  return a.date.localeCompare(b.date) || a.hour - b.hour || a.role.localeCompare(b.role) ||
    a.pid - b.pid || a.seq - b.seq || a.path.localeCompare(b.path);
}

/** `logs/<role>/<date>/rejected.jsonl`, relative to `LOG_DIR`: next to the file the line came from. */
export function quarantinePath(source: SourceFile): string {
  return `${source.role}/${source.date}/rejected.jsonl`;
}
