/**
 * The retention sweeper (plan items 24 and 36): the half of retention that touches the disk.
 *
 * `core/logging/rotation.plan` (items 06, 12) decides, purely and property-tested, which files to
 * gzip, delete and keep. Item 36 put a gate in front of it (`rotation.retentionBlock`, ADR 0007): no
 * event-log file is gzipped or deleted unless it is closed, settled, still the file that was read,
 * and fully accounted for by **committed** ingest. This module gathers the facts that gate judges,
 * pins every file that fails it, and executes what is left. Deleting files is the risky half, so the
 * shape is: *inventory → gate → plan → safety rails → validate → apply*, with every step before
 * "apply" side-effect free, and `dryRun` stopping right after validation.
 *
 * ```text
 *   inventory(root) ─> gate each candidate ─> rotation.plan(pinned) ─> rails ─> validatePlan ─> apply
 *     (read only)       (read + hash, ledger)   (pure; loops until no      (open,    (containment,   (delete,
 *                                                new candidate appears)     .tmp)     verified)        gzip, rmdir)
 * ```
 *
 * ## The gate (item 36)
 *
 * A sweep needs an `IngestLedger`: the committed `_ingest_state` rows (read *after* the ingest that
 * precedes the sweep committed) and the accepted `source_offset`s of the derived store. This module
 * never opens DuckDB — it runs in the server and the CLI too — so the ledger is supplied by the
 * caller; `eventlog/ingest.ts`'s ingest-then-sweep coordinator is the one live supplier.
 * Without a ledger there is no sweep.
 *
 * Each event file `plan` wants to gzip or delete is read in full (decompressed, for a `.gz`) and
 * judged by `retentionBlock`: closed (not open here; dated before today, which no sink can write to
 * again; mtime older than `settleMs`), unchanged since the inventory (size, mtime, inode, before and
 * after the read), watermark covering the whole content with a matching hash, and — for a source
 * never rewritten — every record offset accounted for exactly once by an accepted row or a durable
 * quarantine record in its directory's `rejected.jsonl`. A file that fails is **pinned**: kept,
 * still counted toward `maxTotalBytes`, and reported as `blocked` (the unsafe-to-delete backlog).
 * Pinning can make the cap reach a newer directory, whose files are then judged too; the loop ends
 * when a plan names no file that has not been judged. A `rejected.jsonl` is pinned
 * (`quarantine-evidence`) while any event file in its directory is not being deleted.
 *
 * ## Safety rails
 *
 * - **Containment.** Every planned path is `resolve`d and must sit strictly under the resolved
 *   `LOG_DIR`, and must have exactly the shape `<root>/<role>/<YYYY-MM-DD>/<file>` with a known
 *   role and an event-log file name. So `LOG_DIR/duckdb/events.duckdb`, `LOG_DIR/k6/…` and anything
 *   outside the root are refused even if a plan names them. A dated directory that is a symlink
 *   out of the tree is refused by a `realPath` check on each file's parent. One bad path refuses
 *   the *whole* plan before anything is touched, and emits an `error` lifecycle event.
 * - **Verified only.** `applyPlan` refuses (`unverified`) any event file in `gzip` or `delete`
 *   that the gate did not pass, so a plan built by hand cannot bypass it.
 * - **Today's directory** (and anything dated later) is never in a plan; `validatePlan` re-checks
 *   that independently of `rotation.plan` as defence in depth. A future-dated directory (clock skew,
 *   a restored backup) is held out of `rotation.plan` entirely and reported as `futureDated`, so it
 *   can neither be touched nor make `validatePlan` refuse the rest of the sweep.
 * - **Resolved paths only.** `applyPlan` normalises every planned path with `resolve` and hands
 *   *only* that to the filesystem and to the lifecycle event. `resolve` is lexical, so a raw
 *   `LOG_DIR/server/link/../<date>/<file>` that was validated as `LOG_DIR/server/<date>/<file>`
 *   would otherwise be executed by the OS as "follow `link`, then go up" — somewhere else entirely.
 * - **The open file.** The caller passes the paths its sink holds open (`FileSink.openPath`); they
 *   are pinned whatever the plan said. A sweep run from another process relies on the closed rule:
 *   a sink only ever appends while the clock is inside its file's hour bucket and never reopens a
 *   file it rolled away from, so a file dated before today and settled can receive no more bytes.
 * - **Recheck before every destructive step.** Immediately before removing a file (a plain delete, or
 *   the source after its gzip is verified) its parent directory is `realPath`ed again and must still
 *   be inside the real `LOG_DIR`, and the file must still be the regular file, size, mtime and inode
 *   the inventory saw; a gzip also re-hashes the source against the gate's hash. An append, a swap or
 *   a symlink trick between the gate and the delete is a skip, not a loss. (A check-then-unlink race
 *   can only be narrowed, not removed, without `openat`; the closed rule is what makes it moot.)
 * - **Gzip is atomic and verified.** `<file>.gz.<pid>.tmp` is written and fsynced, renamed to
 *   `<file>.gz`, decompressed and hashed against the source, the directory is fsynced, and only then
 *   is the source removed. Byte offsets survive compression: the `.gz` decompresses to exactly the
 *   bytes ingest committed, and the source's `_ingest_state` row is kept (ADR 0007).
 * - **Durability.** Every touched directory is fsynced once more when the sweep is done. A
 *   filesystem that cannot fsync a directory is skipped, not an error.
 * - Anything else in a dated directory (an operator's notes, a subdirectory, a symlink) is never
 *   inventoried or touched, and keeps its directory from being removed.
 *
 * ## Crash and retry (ADR 0007)
 *
 * Every step is idempotent, so a crash anywhere is repaired by the next sweep: a `.tmp` from a gzip
 * that died is deleted once it is an hour old; a complete `.gz` beside its source (crash after the
 * rename) is verified against the gate's hash and the source removed, while a `.gz` that does not
 * match is left with its source for a human; a delete either happened or is retried. Quarantine files
 * are deleted last, and only once their directory holds no event file at all.
 *
 * Only `events-<HH>-<pid>-<seq>.jsonl[.gz]` and the ingest quarantine file `rejected.jsonl` are
 * inventoried. `rejected.jsonl` is never gzipped: ingest appends to it.
 *
 * ## Scheduling
 *
 * `startRetentionSweeper` is a `forkScoped` fiber: one sweep immediately, then one per `every`. It
 * never fails. It needs a ledger like any sweep, so in practice the scheduled path is the reader's
 * ingest RPC (item 32 triggers it, item 33 hosts it), which runs ingest and then this sweep.
 *
 * A dry run that succeeds emits no event: the event sink writes a file, and a dry run must change
 * nothing on disk. A dry run that *fails* still emits its `error` lifecycle event.
 *
 * ## Events
 *
 * A real sweep emits one `lifecycle` event, phase `retention-sweep`, when it did, skipped or was
 * blocked from anything (`warn` if anything was skipped, blocked or future-dated); a sweep with
 * nothing to do emits nothing, so a quiet tree stays quiet once its events are ingested. An
 * interrupted sweep emits `retention-sweep-interrupted`; a refused or failed one
 * `retention-sweep-failed`. Each carries `gzippedCount`, `deletedCount` and `keptCount` next to its
 * path lists, and `blockedCount`/`blockedBytes` next to the first `MAX_LISTED_BLOCKED` blocked files.
 */
import { createHash } from "node:crypto";
import { dirname, join, relative, resolve, sep } from "node:path";
import { Cause, Clock, Data, Duration, Effect, Schedule, type Scope } from "effect";
import { lifecycleEvent, type Role } from "../core/events.ts";
import {
  type CommittedWatermark,
  dateBucketOf,
  type FileEntry,
  type LogRole,
  plan as rotationPlan,
  recordOffsets,
  type RetentionBlock,
  retentionBlock,
  type RetentionPlan,
  type RotationPolicy,
} from "../core/logging/rotation.ts";
import { Config, EventSink, type LoggingConfig } from "./services.ts";

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/**
 * A planned path (or the root itself) failed a safety rail; **nothing was touched**. Like
 * `SinkIoError`, deliberately not in `core/errors.ts`: it is sweeper-internal and surfaces only as
 * an `error` lifecycle event and this typed failure.
 */
export class RetentionPathError extends Data.TaggedError("RetentionPathError")<{
  readonly reason:
    | "root-unsafe"
    | "not-absolute"
    | "outside-root"
    | "unexpected-shape"
    | "not-inventoried"
    | "today-or-future"
    | "symlink-escape"
    | "unverified";
  readonly path: string;
  readonly root: string;
}> {}

/** Reading the log tree failed before anything was changed. */
export class RetentionIoError extends Data.TaggedError("RetentionIoError")<{
  readonly operation: "inventory" | "realpath";
  readonly path: string;
  readonly cause: unknown;
}> {}

/** The ingest ledger could not be read; nothing was changed. */
export class RetentionLedgerError extends Data.TaggedError("RetentionLedgerError")<{
  readonly operation: "watermarks" | "accepted-offsets";
  readonly detail: string;
  readonly cause?: unknown;
}> {}

export type RetentionError = RetentionPathError | RetentionIoError | RetentionLedgerError;

// ---------------------------------------------------------------------------
// Inventory
// ---------------------------------------------------------------------------

const ROLES: readonly LogRole[] = ["server", "reader", "restore", "cli"];
const EVENT_FILE = /^events-\d{2}-\d+-\d{3,}\.jsonl(\.gz)?$/;
const QUARANTINE_FILE = "rejected.jsonl";
/** A gzip in progress: `<file>.gz.<pid>.tmp`. Never matches an ingest source or a finished `.gz`. */
const PARTIAL_FILE = /^events-\d{2}-\d+-\d{3,}\.jsonl\.gz\.\d+\.tmp$/;
/** A `.tmp` older than this belongs to a sweep that died; a younger one may be in progress. */
const STALE_PARTIAL_MS = 60 * 60 * 1000;
/**
 * Default `settleMs`: a file modified more recently than this is not yet judged closed. A sink's last
 * write to a file can begin just before its hour ends; this is ample time for it to land.
 */
export const DEFAULT_SETTLE_MS = 10 * 60 * 1000;
/** How many blocked files a lifecycle event lists before it only counts. */
const MAX_LISTED_BLOCKED = 50;

function isDateBucket(name: string): boolean {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(name);
  if (match === null) return false;
  const [year, month, day] = [Number(match[1]), Number(match[2]), Number(match[3])];
  const date = new Date(year, month - 1, day);
  return date.getFullYear() === year && date.getMonth() === month - 1 && date.getDate() === day;
}

const baseName = (path: string) => path.slice(path.lastIndexOf(sep) + 1);
const isEventFile = (path: string) => EVENT_FILE.test(baseName(path));
const isQuarantine = (path: string) => baseName(path) === QUARANTINE_FILE;

/** One `<root>/<role>/<YYYY-MM-DD>` directory and every name in it, inventoried or not. */
export interface DatedDir {
  readonly path: string;
  readonly role: LogRole;
  readonly date: string;
  readonly names: readonly string[];
}

export interface Inventory {
  /** The resolved `LOG_DIR` everything below is under. */
  readonly root: string;
  /** What `rotation.plan` is fed: event files and quarantine files, `dir` = bare date bucket. */
  readonly entries: readonly FileEntry[];
  /** `.gz.<pid>.tmp` leftovers of a gzip that did not finish. Never fed to `rotation.plan`. */
  readonly partials: readonly FileEntry[];
  readonly dirs: readonly DatedDir[];
}

/** The resolved root, or `RetentionPathError` if it is empty or the filesystem root. */
function resolveRoot(logDir: string): Effect.Effect<string, RetentionPathError> {
  const root = resolve(logDir);
  if (logDir.trim() === "" || dirname(root) === root) {
    return Effect.fail(new RetentionPathError({ reason: "root-unsafe", path: logDir, root }));
  }
  return Effect.succeed(root);
}

/**
 * Walks `<root>/<role>/<YYYY-MM-DD>/` for the four roles. Read only. Symlinks are not followed
 * and not inventoried, a missing role directory is simply empty, and non-date directories are
 * ignored (which is what keeps `logs/k6` and `logs/duckdb` out of the plan).
 */
export function inventory(root: string): Effect.Effect<Inventory, RetentionIoError> {
  return Effect.tryPromise({
    try: async () => {
      const entries: FileEntry[] = [];
      const partials: FileEntry[] = [];
      const dirs: DatedDir[] = [];
      for (const role of ROLES) {
        const rolePath = resolve(root, role);
        const dated: string[] = [];
        try {
          for await (const entry of Deno.readDir(rolePath)) {
            if (entry.isDirectory && !entry.isSymlink && isDateBucket(entry.name)) dated.push(entry.name);
          }
        } catch (cause) {
          if (cause instanceof Deno.errors.NotFound) continue;
          throw cause;
        }
        for (const date of dated.sort()) {
          const dirPath = resolve(rolePath, date);
          const names: string[] = [];
          for await (const entry of Deno.readDir(dirPath)) {
            names.push(entry.name);
            if (!entry.isFile || entry.isSymlink) continue;
            const isEvent = EVENT_FILE.test(entry.name) || entry.name === QUARANTINE_FILE;
            if (!isEvent && !PARTIAL_FILE.test(entry.name)) continue;
            const path = resolve(dirPath, entry.name);
            let info: Deno.FileInfo;
            try {
              info = await Deno.lstat(path);
            } catch (cause) {
              // Ingest or another sweep may remove a file between the listing and the stat.
              if (cause instanceof Deno.errors.NotFound) continue;
              throw cause;
            }
            if (!info.isFile) continue;
            const file: FileEntry = {
              path,
              dir: date,
              bytes: info.size,
              mtime: info.mtime ?? new Date(0),
              ...(info.ino === null ? {} : { ino: info.ino }),
            };
            (isEvent ? entries : partials).push(file);
          }
          dirs.push({ path: dirPath, role, date, names: names.sort() });
        }
      }
      return { root, entries, partials, dirs };
    },
    catch: (cause) => new RetentionIoError({ operation: "inventory", path: root, cause }),
  });
}

// ---------------------------------------------------------------------------
// The gate: facts for `rotation.retentionBlock`
// ---------------------------------------------------------------------------

/**
 * What committed ingest knows, as the gate needs it. Supplied by the process that owns the event-log
 * DuckDB file (the reader's ingest-then-sweep coordinator); read only after that ingest committed.
 */
export interface IngestLedger {
  /** The committed `_ingest_state` rows, keyed by source path relative to `LOG_DIR` (`/`-separated). */
  readonly watermarks: ReadonlyMap<string, CommittedWatermark>;
  /** Every `platform_events.source_offset` ingested from each of `sources` (relative paths). */
  readonly acceptedOffsets: (
    sources: readonly string[],
  ) => Effect.Effect<ReadonlyMap<string, ReadonlySet<number>>, RetentionLedgerError>;
}

/** What the gate proved about a file it passed: its content's hash and length. */
export interface VerifiedSource {
  readonly sha256: string;
  readonly bytes: number;
}

/** A file `rotation.plan` wanted to gzip or delete that the gate (or the sink) kept. */
export interface BlockedSource {
  /** Relative to `LOG_DIR`, `/`-separated. */
  readonly path: string;
  readonly action: "gzip" | "delete";
  readonly reason: RetentionBlock;
  readonly bytes: number;
}

/** `relative(root, path)`, `/`-separated: the form `_ingest_state.source_file` uses. */
function relPath(root: string, path: string): string {
  return relative(root, path).split(sep).join("/");
}

/** The ingest source a file holds: itself, or for `<source>.gz` the source it was compressed from. */
function sourceOf(relativePath: string): string {
  return relativePath.endsWith(".gz") ? relativePath.slice(0, -3) : relativePath;
}

/** Same regular file, size, mtime and (when known) inode as the inventory recorded. */
function sameIdentity(info: Deno.FileInfo, entry: FileEntry): boolean {
  return info.isFile && info.size === entry.bytes && info.mtime?.getTime() === entry.mtime.getTime() &&
    (entry.ino === undefined || info.ino === null || info.ino === entry.ino);
}

async function lstatOrUndefined(path: string): Promise<Deno.FileInfo | undefined> {
  try {
    return await Deno.lstat(path);
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return undefined;
    throw cause;
  }
}

async function readAll(stream: ReadableStream<Uint8Array>): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  let length = 0;
  for await (const chunk of stream) {
    chunks.push(chunk);
    length += chunk.byteLength;
  }
  const out = new Uint8Array(length);
  let at = 0;
  for (const chunk of chunks) {
    out.set(chunk, at);
    at += chunk.byteLength;
  }
  return out;
}

/**
 * Reads `entry` in full — decompressed, for a `.gz` — and reports whether it stayed the inventoried
 * file across the read. `content` is `undefined` when it cannot be read (or is not valid gzip).
 */
async function readSource(entry: FileEntry): Promise<{ content?: Uint8Array; unchanged: boolean }> {
  const before = await lstatOrUndefined(entry.path);
  if (before === undefined || !sameIdentity(before, entry)) return { unchanged: false };
  let content: Uint8Array | undefined;
  try {
    const file = await Deno.open(entry.path, { read: true });
    content = await readAll(
      entry.path.endsWith(".gz") ? file.readable.pipeThrough(new DecompressionStream("gzip")) : file.readable,
    );
  } catch {
    content = undefined;
  }
  const after = await lstatOrUndefined(entry.path);
  return { content, unchanged: after !== undefined && sameIdentity(after, entry) };
}

/**
 * The `sourceOffset`s of each source's quarantined records in one `rejected.jsonl`, keyed by
 * relative source path. A line that does not parse (a torn quarantine write) proves nothing and is
 * skipped; duplicates (a quarantine written before a commit that then failed) collapse.
 */
async function quarantinedOffsets(path: string): Promise<Map<string, Set<number>>> {
  const out = new Map<string, Set<number>>();
  let text: string;
  try {
    text = await Deno.readTextFile(path);
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return out;
    throw cause;
  }
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    try {
      const record = JSON.parse(line) as { sourceFile?: unknown; sourceOffset?: unknown };
      if (typeof record.sourceFile !== "string" || !Number.isSafeInteger(record.sourceOffset)) continue;
      const offsets = out.get(record.sourceFile) ?? new Set<number>();
      offsets.add(record.sourceOffset as number);
      out.set(record.sourceFile, offsets);
    } catch {
      // Not evidence.
    }
  }
  return out;
}

const NO_OFFSETS: ReadonlySet<number> = new Set();

function sha256Hex(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

interface GateContext {
  readonly root: string;
  readonly now: Date;
  readonly settleMs: number;
  readonly open: ReadonlySet<string>;
  readonly ledger: IngestLedger;
}

/**
 * Judges `entries` (event files only): `VerifiedSource` for each that passes, the block reason for
 * each that does not. One `acceptedOffsets` call covers every source that needs reconciling.
 */
function judge(
  context: GateContext,
  entries: readonly FileEntry[],
): Effect.Effect<Map<string, VerifiedSource | RetentionBlock>, RetentionLedgerError | RetentionIoError> {
  return Effect.gen(function* () {
    const { root, ledger } = context;
    const quarantines = new Map<string, Map<string, Set<number>>>();
    const gathered = yield* Effect.forEach(entries, (entry) =>
      Effect.tryPromise({
        try: async () => {
          const source = sourceOf(relPath(root, entry.path));
          const quarantine = join(dirname(entry.path), QUARANTINE_FILE);
          let rejectedBySource = quarantines.get(quarantine);
          if (rejectedBySource === undefined) {
            rejectedBySource = await quarantinedOffsets(quarantine);
            quarantines.set(quarantine, rejectedBySource);
          }
          const read = context.open.has(entry.path) ? { unchanged: true } : await readSource(entry);
          const content = read.content === undefined ? undefined : {
            bytes: read.content.byteLength,
            sha256: sha256Hex(read.content),
            recordOffsets: recordOffsets(read.content),
          };
          return {
            entry,
            source,
            content,
            unchanged: read.unchanged,
            watermark: ledger.watermarks.get(source),
            rejected: rejectedBySource.get(source) ?? NO_OFFSETS,
          };
        },
        catch: (cause) => new RetentionIoError({ operation: "inventory", path: entry.path, cause }),
      }));

    // Only a source whose watermark already vouches for its bytes, and that was never rewritten,
    // needs its records reconciled against the derived store.
    const toReconcile = gathered.filter((g) =>
      g.content !== undefined && g.watermark !== undefined && !g.watermark.dedupe &&
      g.watermark.bytesIngested === g.content.bytes && g.watermark.prefixSha256 === g.content.sha256
    ).map((g) => g.source);
    const accepted = toReconcile.length === 0
      ? new Map<string, ReadonlySet<number>>()
      : yield* ledger.acceptedOffsets([...new Set(toReconcile)]);

    const verdicts = new Map<string, VerifiedSource | RetentionBlock>();
    for (const g of gathered) {
      const block = retentionBlock(
        {
          open: context.open.has(g.entry.path),
          mtime: g.entry.mtime,
          unchanged: g.unchanged,
          content: g.content,
          watermark: g.watermark,
          accepted: accepted.get(g.source) ?? NO_OFFSETS,
          rejected: g.rejected,
        },
        context.now,
        context.settleMs,
      );
      verdicts.set(
        g.entry.path,
        block ?? { sha256: g.content!.sha256, bytes: g.content!.bytes },
      );
    }
    return verdicts;
  });
}

// ---------------------------------------------------------------------------
// Plan: rotation.plan over the pinned set, then the rails rotation cannot know
// ---------------------------------------------------------------------------

/**
 * Applies the rails that depend on things `rotation.plan` cannot see:
 * - a path some sink holds open is kept, whatever the plan said;
 * - `rejected.jsonl` is never gzipped (it ages out with its directory but ingest appends to it).
 *
 * Pure. `openPaths` are compared after `resolve`, so a relative or unnormalised path still matches.
 */
export function applySafetyRails(plan: RetentionPlan, openPaths: readonly string[]): RetentionPlan {
  const open = new Set(openPaths.map((p) => resolve(p)));
  const gzip: string[] = [];
  const del: string[] = [];
  const keep = new Set(plan.keep);
  for (const path of plan.gzip) {
    if (open.has(resolve(path)) || isQuarantine(path)) keep.add(path);
    else gzip.push(path);
  }
  for (const path of plan.delete) {
    if (open.has(resolve(path))) keep.add(path);
    else del.push(path);
  }
  return { gzip, delete: del, keep: [...keep].sort() };
}

/** Dated directories that would hold nothing once `deleted` (absolute paths) are gone. */
export function emptiedDirs(
  dirs: readonly DatedDir[],
  deleted: ReadonlySet<string>,
  today: string,
): readonly string[] {
  return dirs
    .filter((d) => d.date < today && d.names.every((name) => deleted.has(resolve(d.path, name))))
    .map((d) => d.path)
    .sort();
}

export interface PlanSweepOptions {
  readonly root: string;
  readonly inventory: Inventory;
  readonly now: Date;
  readonly policy: RotationPolicy;
  readonly ledger: IngestLedger;
  /** Absolute paths this process's sinks hold open. */
  readonly openPaths: readonly string[];
  readonly settleMs: number;
}

/** A gated plan, ready for `applyPlan`. All paths absolute. */
export interface GatedPlan {
  readonly plan: RetentionPlan;
  readonly verified: ReadonlyMap<string, VerifiedSource>;
  readonly blocked: readonly BlockedSource[];
  readonly today: string;
}

/**
 * `rotation.plan` with every file the gate refuses pinned. Iterates because a pin can push the cap
 * onto a newer directory: each round judges only files no earlier round judged, so it ends. Reads
 * files; changes nothing.
 */
export function planSweep(
  options: PlanSweepOptions,
): Effect.Effect<GatedPlan, RetentionLedgerError | RetentionIoError> {
  return Effect.gen(function* () {
    const { root, inventory: inv, now, policy } = options;
    const today = dateBucketOf(now);
    const open = new Set(options.openPaths.map((p) => resolve(p)));
    const context: GateContext = { root, now, settleMs: options.settleMs, open, ledger: options.ledger };
    // A directory dated after today (clock skew, a restored backup) is not retention's to judge:
    // `rotation.plan` would let the size cap delete it, `validatePlan` would then refuse the whole
    // plan, and one stray directory would stop every other file from ever being swept.
    const entries = inv.entries.filter((e) => e.dir <= today);
    const byPath = new Map(entries.map((e) => [e.path, e]));

    const pins = new Map<string, RetentionBlock>();
    for (const entry of entries) if (open.has(entry.path)) pins.set(entry.path, "open");
    const verdicts = new Map<string, VerifiedSource | RetentionBlock>();
    /** Every path some round's plan wanted to act on, and the action it wanted first. */
    const wanted = new Map<string, "gzip" | "delete">();
    const note = (p: RetentionPlan) => {
      for (const path of p.gzip) if (!wanted.has(path)) wanted.set(path, "gzip");
      for (const path of p.delete) if (!wanted.has(path)) wanted.set(path, "delete");
    };
    note(rotationPlan(entries, now, policy));

    let planned: RetentionPlan;
    while (true) {
      planned = rotationPlan(entries, now, policy, new Set(pins.keys()));
      note(planned);
      const fresh = [...planned.gzip, ...planned.delete]
        .filter((path) => isEventFile(path) && !verdicts.has(path))
        .map((path) => byPath.get(path)!);
      if (fresh.length > 0) {
        for (const [path, verdict] of yield* judge(context, fresh)) {
          verdicts.set(path, verdict);
          if (typeof verdict === "string") pins.set(path, verdict);
        }
        continue;
      }
      // A quarantine file holds the evidence that its directory's rejected lines were consumed; it
      // goes only together with every event file beside it.
      const deleting = new Set(planned.delete);
      let pinnedEvidence = false;
      for (const path of planned.delete) {
        if (!isQuarantine(path) || pins.has(path)) continue;
        const dir = dirname(path);
        if (entries.some((e) => isEventFile(e.path) && dirname(e.path) === dir && !deleting.has(e.path))) {
          pins.set(path, "quarantine-evidence");
          pinnedEvidence = true;
        }
      }
      if (!pinnedEvidence) break;
    }

    const verified = new Map<string, VerifiedSource>();
    for (const [path, verdict] of verdicts) if (typeof verdict !== "string") verified.set(path, verdict);
    const blocked: BlockedSource[] = [...pins]
      .filter(([path]) => wanted.has(path))
      .map(([path, reason]) => ({
        path: relPath(root, path),
        action: wanted.get(path)!,
        reason,
        bytes: byPath.get(path)!.bytes,
      }))
      .sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));

    const railed = applySafetyRails(planned, [...open]);
    // A leftover `.tmp` from a gzip that died is garbage once it is old enough that nothing can
    // still be writing it — except in today's directory, which is never touched.
    const stale = inv.partials
      .filter((p) => p.dir < today && now.getTime() - p.mtime.getTime() >= STALE_PARTIAL_MS)
      .map((p) => p.path);
    const future = inv.entries.filter((e) => e.dir > today).map((e) => e.path);
    return {
      plan: {
        gzip: railed.gzip,
        delete: [...railed.delete, ...stale].sort(),
        keep: [...railed.keep, ...future].sort(),
      },
      verified,
      blocked,
      today,
    };
  });
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

/**
 * Refuses the plan unless every path in `gzip` and `delete` is an inventoried file of the right
 * shape strictly inside `root`, outside today's (and later) directories, not reached through a
 * symlinked directory, and — when `verified` is given — an event file the gate passed. Fails with
 * the first offender; the caller has changed nothing yet.
 */
export function validatePlan(
  root: string,
  plan: Pick<RetentionPlan, "gzip" | "delete">,
  known: ReadonlySet<string>,
  today: string,
  verified?: ReadonlySet<string>,
): Effect.Effect<void, RetentionPathError | RetentionIoError> {
  const refuse = (reason: RetentionPathError["reason"], path: string) =>
    Effect.fail(new RetentionPathError({ reason, path, root }));
  return Effect.gen(function* () {
    const rootReal = yield* realPath(root);
    for (const path of [...plan.gzip, ...plan.delete]) {
      if (!path.startsWith(sep)) return yield* refuse("not-absolute", path);
      const abs = resolve(path);
      if (!abs.startsWith(root + sep)) return yield* refuse("outside-root", path);
      const [role, date, name, ...extra] = relative(root, abs).split(sep);
      const shaped = ROLES.includes(role as LogRole) && isDateBucket(date ?? "") && name !== undefined &&
        extra.length === 0 &&
        (EVENT_FILE.test(name) || name === QUARANTINE_FILE || PARTIAL_FILE.test(name));
      if (!shaped) return yield* refuse("unexpected-shape", path);
      if (date >= today) return yield* refuse("today-or-future", path);
      if (!known.has(abs)) return yield* refuse("not-inventoried", path);
      if (verified !== undefined && EVENT_FILE.test(name) && !verified.has(abs)) {
        return yield* refuse("unverified", path);
      }
      const parentReal = yield* realPath(dirname(abs));
      if (parentReal !== undefined && rootReal !== undefined && !parentReal.startsWith(rootReal + sep)) {
        return yield* refuse("symlink-escape", path);
      }
    }
  });
}

/** `realPath`, or `undefined` if the path is already gone (an absent file is handled at apply). */
function realPath(path: string): Effect.Effect<string | undefined, RetentionIoError> {
  return Effect.tryPromise({
    try: async () => {
      try {
        return await Deno.realPath(path);
      } catch (cause) {
        if (cause instanceof Deno.errors.NotFound) return undefined;
        throw cause;
      }
    },
    catch: (cause) => new RetentionIoError({ operation: "realpath", path, cause }),
  });
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

/** A planned action that was not carried out, and why. Not an error: the sweep carries on. */
export interface SkippedAction {
  readonly path: string;
  readonly action: "gzip" | "delete" | "rmdir" | "fsync";
  readonly reason: string;
}

/** What a sweep did. All paths are relative to `LOG_DIR` and `/`-separated. */
export interface SweepReport {
  readonly dryRun: boolean;
  /** The plan after the gate and the rails; with `dryRun` this is everything that *would* happen. */
  readonly plan: RetentionPlan;
  /** Dated directories the plan would leave empty (and a real sweep removes). */
  readonly emptiedDirs: readonly string[];
  /** Dated directories newer than today: held out of the plan, never touched, and no obstacle. */
  readonly futureDated: readonly string[];
  /** Files the policy wanted gone or compressed that the gate kept: the unsafe-to-delete backlog. */
  readonly blocked: readonly BlockedSource[];
  readonly blockedBytes: number;
  /** What actually happened. All empty on a dry run. */
  readonly gzipped: readonly string[];
  readonly deleted: readonly string[];
  readonly removedDirs: readonly string[];
  readonly skipped: readonly SkippedAction[];
}

async function sha256Of(stream: ReadableStream<Uint8Array>): Promise<string> {
  const hash = createHash("sha256");
  for await (const chunk of stream) hash.update(chunk);
  return hash.digest("hex");
}

async function digestFile(path: string): Promise<string> {
  const file = await Deno.open(path, { read: true });
  return sha256Of(file.readable);
}

/** SHA-256 of what `path` decompresses to; rejects if it is not a complete gzip stream. */
async function digestGzip(path: string): Promise<string> {
  const file = await Deno.open(path, { read: true });
  return sha256Of(file.readable.pipeThrough(new DecompressionStream("gzip")));
}

async function compressTo(src: string, tmp: string, mtime: Date): Promise<void> {
  const input = await Deno.open(src, { read: true });
  const output = await Deno.open(tmp, { write: true, createNew: true });
  try {
    for await (const chunk of input.readable.pipeThrough(new CompressionStream("gzip"))) {
      let written = 0;
      while (written < chunk.byteLength) written += await output.write(chunk.subarray(written));
    }
    await output.syncData();
  } finally {
    output.close();
  }
  // The `.gz` keeps the original's mtime so file age stays meaningful after compression.
  await Deno.utime(tmp, mtime, mtime);
}

async function exists(path: string): Promise<boolean> {
  return (await lstatOrUndefined(path)) !== undefined;
}

/** Why `path` is no longer the file the inventory saw, or `undefined` if it still is. */
async function changedSince(path: string, entry: FileEntry): Promise<string | undefined> {
  const info = await lstatOrUndefined(path);
  if (info === undefined) return "missing";
  if (!info.isFile) return "not a regular file";
  if (!sameIdentity(info, entry)) return "changed since inventory";
  return undefined;
}

/** What the destructive steps recheck against, taken once after the plan validated. */
interface Guard {
  readonly root: string;
  /** `realPath(root)`, or `undefined` if the root has vanished. */
  readonly rootReal: string | undefined;
}

/**
 * Why `path` may no longer be removed, or `undefined` if it still may: it is not a resolved path,
 * its parent directory no longer resolves to where the inventory put it (a symlink was swapped in),
 * or it is no longer the file the inventory saw. Called immediately before each removal.
 */
async function recheck(guard: Guard, path: string, entry: FileEntry): Promise<string | undefined> {
  if (resolve(path) !== path) return "path is not resolved";
  if (guard.rootReal === undefined) return "log root is gone";
  let parentReal: string;
  try {
    parentReal = await Deno.realPath(dirname(path));
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return "missing";
    throw cause;
  }
  if (parentReal !== resolve(guard.rootReal, relative(guard.root, dirname(path)))) {
    return "parent directory is not where the inventory found it";
  }
  return changedSince(path, entry);
}

/** `fsync` returns these when a filesystem (or platform) cannot sync a directory at all. */
const DIR_SYNC_UNSUPPORTED = new Set(["EINVAL", "ENOSYS", "ENOTSUP", "EOPNOTSUPP", "EISDIR", "EPERM"]);

/**
 * fsyncs directory `path` so renames and unlinks inside it are durable. A filesystem or platform
 * that cannot sync a directory (or a directory that is already gone) is not a problem: `undefined`.
 * Anything else is an I/O note for the caller to report. Exported for the unit tests and for
 * ingest's quarantine durability.
 */
export async function syncDir(path: string): Promise<string | undefined> {
  const unsupported = (cause: unknown) => {
    const code = (cause as { code?: unknown } | null)?.code;
    return cause instanceof Deno.errors.PermissionDenied ||
      (typeof code === "string" && DIR_SYNC_UNSUPPORTED.has(code));
  };
  const note = (cause: unknown) =>
    unsupported(cause) ? undefined : `io error: ${cause instanceof Error ? cause.message : String(cause)}`;
  let dir: Deno.FsFile;
  try {
    dir = await Deno.open(path, { read: true });
  } catch (cause) {
    return cause instanceof Deno.errors.NotFound ? undefined : note(cause);
  }
  try {
    await dir.sync();
    return undefined;
  } catch (cause) {
    return note(cause);
  } finally {
    dir.close();
  }
}

type Operation = (
  path: string,
  entry: FileEntry,
  guard: Guard,
  verified: VerifiedSource | undefined,
) => Promise<string | undefined>;

/** Returns a skip reason, or `undefined` once `path` is gone. Never throws. */
const deleteOne: Operation = async (path, entry, guard) => {
  try {
    const refused = await recheck(guard, path, entry);
    if (refused !== undefined) return refused;
    await Deno.remove(path);
    return undefined;
  } catch (cause) {
    return `io error: ${cause instanceof Error ? cause.message : String(cause)}`;
  }
};

/** Deletes a quarantine file only once no event file is left beside it. */
const deleteQuarantine: Operation = async (path, entry, guard) => {
  try {
    for await (const sibling of Deno.readDir(dirname(path))) {
      if (EVENT_FILE.test(sibling.name)) return "an event file it holds evidence for is still present";
    }
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return "missing";
    return `io error: ${cause instanceof Error ? cause.message : String(cause)}`;
  }
  return deleteOne(path, entry, guard, undefined);
};

/**
 * The last step of a gzip: removes the source once its `.gz` is verified. The directory is synced
 * first so the rename cannot be lost while the removal survives, and the path is rechecked
 * immediately before the unlink, since the gzip and its verification took real time.
 */
async function removeVerifiedSource(
  guard: Guard,
  path: string,
  entry: FileEntry,
): Promise<string | undefined> {
  const note = await syncDir(dirname(path));
  if (note !== undefined) return `gzip not durable: ${note}`;
  const refused = await recheck(guard, path, entry);
  if (refused !== undefined) return refused;
  await Deno.remove(path);
  return undefined;
}

/** Returns a skip reason, or `undefined` once `<path>.gz` exists, verified, and `path` is gone. */
const gzipOne: Operation = async (path, entry, guard, verified) => {
  const dst = `${path}.gz`;
  const tmp = `${dst}.${Deno.pid}.tmp`;
  try {
    const refused = await recheck(guard, path, entry);
    if (refused !== undefined) return refused;
    const expected = await digestFile(path);
    // The bytes the gate proved ingested, and nothing else, are what may be compressed.
    if (verified === undefined || expected !== verified.sha256) return "changed since verification";
    if (await exists(dst)) {
      // Left by an interrupted sweep (rename done, source not yet removed). Trust it only if it
      // decompresses to exactly the source; otherwise leave both files for a human.
      const actual = await digestGzip(dst).catch(() => undefined);
      if (actual !== expected) return "a different .gz already exists";
      return await removeVerifiedSource(guard, path, entry);
    }
    try {
      await compressTo(path, tmp, entry.mtime);
      await Deno.rename(tmp, dst);
    } catch (cause) {
      await Deno.remove(tmp).catch(() => {});
      throw cause;
    }
    if (await digestGzip(dst) !== expected) {
      await Deno.remove(dst);
      return "gzip verification failed";
    }
    return await removeVerifiedSource(guard, path, entry);
  } catch (cause) {
    return `io error: ${cause instanceof Error ? cause.message : String(cause)}`;
  }
};

async function removeEmptyDir(path: string): Promise<string | undefined> {
  try {
    // Non-recursive on purpose: a directory that gained a file since the inventory is left alone.
    await Deno.remove(path);
    return undefined;
  } catch (cause) {
    if (cause instanceof Deno.errors.NotFound) return undefined;
    return `io error: ${cause instanceof Error ? cause.message : String(cause)}`;
  }
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

const PHASE = "retention-sweep";

function emitLifecycle(
  role: Role,
  level: "info" | "warn" | "error",
  phase: string,
  detail: unknown,
): Effect.Effect<void, never, EventSink> {
  return Effect.gen(function* () {
    const sink = yield* EventSink;
    const ts = new Date(yield* Clock.currentTimeMillis).toISOString();
    yield* sink.emit(lifecycleEvent({
      eventId: crypto.randomUUID(),
      ts,
      level,
      role,
      pid: Deno.pid,
      traceId: `retention-${crypto.randomUUID()}`,
      phase,
      detail,
    }));
  });
}

/** What a sweep changed: `kept` is every inventoried file left exactly as it was found. */
interface Counts {
  readonly gzippedCount: number;
  readonly deletedCount: number;
  readonly keptCount: number;
}

/** Counts for a failure before anything was inventoried or touched. */
const NOTHING_DONE: Counts = { gzippedCount: 0, deletedCount: 0, keptCount: 0 };

function emitFailure(
  role: Role,
  root: string,
  error: RetentionError,
  counts: Counts = NOTHING_DONE,
): Effect.Effect<void, never, EventSink> {
  const { _tag: errorTag, ...fields } = error;
  return emitLifecycle(role, "error", `${PHASE}-failed`, {
    errorTag,
    root,
    ...fields,
    ...counts,
    // `cause` is a non-enumerable property on tagged errors and a foreign object besides.
    cause: "cause" in error ? String(error.cause) : undefined,
  });
}

/** The blocked backlog as an event carries it: counted in full, listed up to a bound. */
function blockedDetail(blocked: readonly BlockedSource[]) {
  return {
    blocked: blocked.slice(0, MAX_LISTED_BLOCKED),
    blockedCount: blocked.length,
    blockedBytes: blocked.reduce((sum, b) => sum + b.bytes, 0),
  };
}

// ---------------------------------------------------------------------------
// Sweep
// ---------------------------------------------------------------------------

export interface ApplyOptions {
  /** The resolved `LOG_DIR`. */
  readonly root: string;
  readonly inventory: Inventory;
  /** What to do, as absolute paths. `gzip` and `delete` are validated in full before any change. */
  readonly plan: RetentionPlan;
  /** Event files the gate passed (absolute paths). Any other event file in the plan is refused. */
  readonly verified: ReadonlyMap<string, VerifiedSource>;
  /** The gate's backlog, carried into the report and the event. */
  readonly blocked?: readonly BlockedSource[];
  readonly today: string;
  readonly role: Role;
  readonly dryRun?: boolean;
}

/**
 * Validates `plan` against `root`, the inventory and the gate's verdicts, then executes it: delete
 * event files and stale partials, then gzip (so a full disk frees space first), then delete
 * quarantine files left with no event file beside them, then remove emptied dated directories, then
 * fsync what was touched. A path that fails a safety rail refuses the whole plan: an `error`
 * lifecycle event is emitted and `RetentionPathError` returned, with nothing changed. Per-file
 * problems (a file that changed, an I/O error) are skipped and reported, not fatal.
 *
 * Validation sees the caller's spelling of each path; execution sees only its `resolve`d form, so
 * what was validated is exactly what is touched and what the events name.
 *
 * Emits one `lifecycle` event naming exactly what was gzipped, deleted and blocked, with counts, when
 * there was anything to name. None on a successful `dryRun` (a failed one still emits its `error`
 * event). If the fiber is interrupted mid-apply, a `retention-sweep-interrupted` event records the
 * state reached, including the one file whose operation was in flight; that operation, being
 * crash-safe, may still finish.
 */
export function applyPlan(
  options: ApplyOptions,
): Effect.Effect<SweepReport, RetentionPathError | RetentionIoError, EventSink> {
  const { root, role, today, inventory: inv } = options;
  const rel = (path: string) => relPath(root, path);
  const byPath = new Map([...inv.entries, ...inv.partials].map((e) => [e.path, e]));
  const inventoried = byPath.size;
  const blocked = options.blocked ?? [];
  const blockedBytes = blocked.reduce((sum, b) => sum + b.bytes, 0);

  return Effect.gen(function* () {
    const verified = new Map([...options.verified].map(([path, v]) => [resolve(path), v]));
    yield* validatePlan(root, options.plan, new Set(byPath.keys()), today, new Set(verified.keys()));
    // From here on only these resolved paths exist: `link/../<date>/<file>` passed validation as
    // `<date>/<file>`, and the OS must never be given the spelling it would resolve differently.
    const resolved = (paths: readonly string[]) => [...new Set(paths.map((p) => resolve(p)))].sort();
    const plan: RetentionPlan = {
      gzip: resolved(options.plan.gzip),
      delete: resolved(options.plan.delete),
      keep: resolved(options.plan.keep),
    };
    const emptied = emptiedDirs(inv.dirs, new Set(plan.delete), today);
    const futureDated = inv.dirs.filter((d) => d.date > today).map((d) => rel(d.path)).sort();
    const reportPlan: RetentionPlan = {
      gzip: plan.gzip.map(rel),
      delete: plan.delete.map(rel),
      keep: plan.keep.map(rel),
    };

    if (options.dryRun === true) {
      return {
        dryRun: true,
        plan: reportPlan,
        emptiedDirs: emptied.map(rel),
        futureDated,
        blocked,
        blockedBytes,
        gzipped: [],
        deleted: [],
        removedDirs: [],
        skipped: [],
      } satisfies SweepReport;
    }

    const guard: Guard = { root, rootReal: yield* realPath(root) };
    const skipped: SkippedAction[] = [];
    const deleted: string[] = [];
    const gzipped: string[] = [];
    const removedDirs: string[] = [];
    let inFlight: string | undefined;
    const counts = (): Counts => ({
      gzippedCount: gzipped.length,
      deletedCount: deleted.length,
      keptCount: inventoried - gzipped.length - deleted.length,
    });
    const run = (action: "gzip" | "delete", paths: readonly string[], op: Operation, done: string[]) =>
      Effect.forEach(paths, (path) =>
        Effect.promise(() => {
          inFlight = rel(path);
          return op(path, byPath.get(path)!, guard, verified.get(path));
        }).pipe(
          Effect.map((reason) => {
            inFlight = undefined;
            if (reason === undefined) done.push(rel(path));
            else skipped.push({ path: rel(path), action, reason });
          }),
        ), { discard: true });

    const apply = Effect.gen(function* () {
      yield* run("delete", plan.delete.filter((p) => !isQuarantine(p)), deleteOne, deleted);
      yield* run("gzip", plan.gzip, gzipOne, gzipped);
      yield* run("delete", plan.delete.filter(isQuarantine), deleteQuarantine, deleted);

      // Directories empty only because of what we actually deleted, plus any that were already empty.
      const goneNow = new Set(deleted.map((p) => resolve(root, p)));
      yield* Effect.forEach(
        emptiedDirs(inv.dirs, goneNow, today),
        (dir) =>
          Effect.promise(() => removeEmptyDir(dir)).pipe(
            Effect.map((reason) => {
              if (reason === undefined) removedDirs.push(rel(dir));
              else skipped.push({ path: rel(dir), action: "rmdir", reason });
            }),
          ),
        { discard: true },
      );

      // Per-file durability was handled in `gzipOne`; this makes the unlinks and rmdirs durable too.
      const touched = new Set(
        [...deleted, ...gzipped, ...removedDirs].map((p) => dirname(resolve(root, p))),
      );
      yield* Effect.forEach(
        [...touched].sort(),
        (dir) =>
          Effect.promise(() => syncDir(dir)).pipe(
            Effect.map((reason) => {
              if (reason !== undefined) skipped.push({ path: rel(dir), action: "fsync", reason });
            }),
          ),
        { discard: true },
      );
    }).pipe(
      Effect.onInterrupt(() =>
        emitLifecycle(role, "warn", `${PHASE}-interrupted`, {
          root,
          interrupted: true,
          inFlight,
          gzipped,
          deleted,
          removedDirs,
          skipped,
          futureDated,
          ...blockedDetail(blocked),
          ...counts(),
        })
      ),
    );
    yield* apply;

    deleted.sort();
    const report: SweepReport = {
      dryRun: false,
      plan: reportPlan,
      emptiedDirs: emptied.map(rel),
      futureDated,
      blocked,
      blockedBytes,
      gzipped,
      deleted,
      removedDirs,
      skipped,
    };
    const eventful = gzipped.length + deleted.length + removedDirs.length + skipped.length +
        blocked.length + futureDated.length > 0;
    if (eventful) {
      const quiet = skipped.length === 0 && blocked.length === 0 && futureDated.length === 0;
      yield* emitLifecycle(role, quiet ? "info" : "warn", PHASE, {
        root,
        gzipped,
        deleted,
        removedDirs,
        skipped,
        futureDated,
        ...blockedDetail(blocked),
        ...counts(),
      });
    }
    return report;
  }).pipe(
    // A refusal happens before the first change: everything inventoried was kept.
    Effect.tapError((error) =>
      emitFailure(role, root, error, { gzippedCount: 0, deletedCount: 0, keptCount: inventoried })
    ),
  );
}

export interface SweepOptions<R = never> {
  /** `Config.logging.dir` (`LOG_DIR`). Resolved here; the root is refused if empty or `/`. */
  readonly logDir: string;
  readonly policy: RotationPolicy;
  /** The role stamped on the `lifecycle` event: `"reader"` for the coordinator, `"cli"` for `logs:sweep`. */
  readonly role: Role;
  /**
   * The committed ingest state the gate judges against, read when the sweep runs (so after any ingest
   * that precedes it). There is no sweep without one: see the module doc.
   */
  readonly ledger: Effect.Effect<IngestLedger, RetentionLedgerError, R>;
  /** Plan and validate, but change nothing. Emits no event unless the sweep fails. */
  readonly dryRun?: boolean;
  /** Paths this process's sink(s) hold open — `FileSink.openPath`. Kept whatever the plan says. */
  readonly openPaths?: Effect.Effect<readonly string[]>;
  /** A file modified more recently than this is not yet closed. Default `DEFAULT_SETTLE_MS`. */
  readonly settleMs?: number;
}

/**
 * One sweep, as of the current `Clock` time: inventory `LOG_DIR`, read the ledger, gate and plan
 * (`planSweep`), validate, then (unless `dryRun`) apply. See the module doc for the rails.
 */
export function sweep<R = never>(
  options: SweepOptions<R>,
): Effect.Effect<SweepReport, RetentionError, EventSink | R> {
  return Effect.gen(function* () {
    const root = yield* resolveRoot(options.logDir).pipe(
      Effect.tapError((e) => emitFailure(options.role, e.root, e)),
    );
    const inv = yield* inventory(root).pipe(
      Effect.tapError((e) => emitFailure(options.role, root, e)),
    );
    const now = new Date(yield* Clock.currentTimeMillis);
    const open = options.openPaths === undefined ? [] : yield* options.openPaths;
    const keptAll: Counts = { gzippedCount: 0, deletedCount: 0, keptCount: inv.entries.length };
    const gated = yield* Effect.flatMap(options.ledger, (ledger) =>
      planSweep({
        root,
        inventory: inv,
        now,
        policy: options.policy,
        ledger,
        openPaths: open,
        settleMs: options.settleMs ?? DEFAULT_SETTLE_MS,
      })).pipe(Effect.tapError((e) => emitFailure(options.role, root, e, keptAll)));

    return yield* applyPlan({
      root,
      inventory: inv,
      plan: gated.plan,
      verified: gated.verified,
      blocked: gated.blocked,
      today: gated.today,
      role: options.role,
      dryRun: options.dryRun,
    });
  });
}

/** `RotationPolicy` from the `LOG_*` configuration. */
export function policyFromConfig(logging: LoggingConfig): RotationPolicy {
  return {
    keepDays: logging.retentionDays,
    gzipAfterDays: logging.gzipAfterDays,
    maxTotalBytes: logging.maxTotalBytes,
    maxFileBytes: logging.maxFileBytes,
  };
}

/** `sweep` over `Config.logging`. This is what the coordinator and the `logs:sweep` CLI run. */
export function sweepConfigured<R = never>(
  options: Pick<SweepOptions<R>, "role" | "dryRun" | "openPaths" | "ledger" | "settleMs">,
): Effect.Effect<SweepReport, RetentionError, Config | EventSink | R> {
  return Effect.flatMap(Config, (config) =>
    sweep({
      ...options,
      logDir: config.logging.dir,
      policy: policyFromConfig(config.logging),
    }));
}

export interface SweeperOptions<R = never>
  extends Pick<SweepOptions<R>, "role" | "openPaths" | "ledger" | "settleMs"> {
  /** Time between sweeps, after the one at startup. Defaults to one day. */
  readonly every?: Duration.DurationInput;
}

/**
 * Sweeps once now, then every `every` (default one day), in a fiber tied to the caller's scope.
 * Never fails: a typed failure is already an `error` event, a defect is reported as one here, and
 * either way the next sweep still runs. Interruption (scope close) is not swallowed.
 */
export function startRetentionSweeper<R = never>(
  options: SweeperOptions<R>,
): Effect.Effect<void, never, Scope.Scope | Config | EventSink | R> {
  const once = sweepConfigured(options).pipe(
    Effect.asVoid,
    Effect.ignore,
    Effect.catchAllCause((cause) =>
      Cause.isInterruptedOnly(cause) ? Effect.failCause(cause) : emitLifecycle(
        options.role,
        "error",
        `${PHASE}-failed`,
        { errorTag: "RetentionDefect", cause: Cause.pretty(cause) },
      )
    ),
  );
  return Effect.asVoid(Effect.forkScoped(
    Effect.repeat(once, Schedule.spaced(options.every ?? Duration.days(1))),
  ));
}
