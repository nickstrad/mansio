/**
 * Pure rotation and retention policy for the JSONL event log (plan item 06).
 *
 * This module is PURE: no platform globals, no built-in runtime modules, no filesystem access. It
 * only computes paths and plans against a policy; the risky part — actually gzipping or deleting
 * files on disk — is left to the sweeper that executes a `RetentionPlan`
 * (`eventlog/retention.ts`). Item 36 adds the gate that sweeper applies first:
 * `retentionBlock` decides, from facts the sweeper gathered, whether a file is closed, settled and
 * fully accounted for by committed ingest, and `plan`'s `pinned` set keeps every file that is not.
 *
 * On-disk layout (see docs/plans/effect-port-and-observability.md §4.2):
 *   logs/<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl
 * `role` is one of the four known roles. `YYYY-MM-DD` and `HH` are the local calendar date and
 * hour at the moment the file was opened (local time, not UTC — logs should read in the operator's
 * wall clock). `seq` is zero-padded to 3 digits, matching the plan's worked example.
 * A gzipped file stays in its dated directory and gains a `.gz` suffix:
 *   logs/<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl.gz
 */
import { Data } from "effect";
import { completeLines } from "../eventlog/offsets.ts";

/** The four processes that write their own event log stream. */
export type LogRole = "server" | "reader" | "restore" | "cli";

const KNOWN_ROLES: ReadonlySet<LogRole> = new Set(["server", "reader", "restore", "cli"]);

/** `role` did not match one of the four known log roles. Roles are code-supplied, never caller input. */
export class InvalidLogRoleError extends Data.TaggedError("InvalidLogRoleError")<{
  readonly role: string;
}> {}

/** A `FileEntry.dir` was not a bare `YYYY-MM-DD` date bucket. Thrown by `plan` so a mis-shaped
 * inventory fails loudly instead of being silently kept forever. */
export class InvalidDateBucketError extends Data.TaggedError("InvalidDateBucketError")<{
  readonly dir: string;
}> {}

/** Rotation and retention thresholds. All byte and day values are inclusive lower bounds for the
 * state they name (e.g. a file exactly `maxFileBytes` old is already due to roll). */
export interface RotationPolicy {
  /** Dated directories older than this (in days, relative to `now`) are deleted outright. */
  readonly keepDays: number;
  /** Dated directories at least this old, but within `keepDays`, are gzipped. */
  readonly gzipAfterDays: number;
  /** Total bytes across all log files — including today's directory — before the oldest dated
   * directories are pruned back under the cap. */
  readonly maxTotalBytes: number;
  /** Bytes written to the open file before it rolls. */
  readonly maxFileBytes: number;
}

const GIB = 1024 ** 3;
const MIB = 1024 ** 2;

export const defaultRotationPolicy: RotationPolicy = {
  keepDays: 14,
  gzipAfterDays: 2,
  maxTotalBytes: 2 * GIB,
  maxFileBytes: 64 * MIB,
};

/** State of the currently-open log file, as tracked by whatever last called `currentPath`. */
export interface RollState {
  /** The full dated directory the open file lives in: `logs/<role>/<YYYY-MM-DD>`. This is a path,
   * not the bare `YYYY-MM-DD` bucket — contrast `FileEntry.dir`, which is the bucket alone. */
  readonly dir: string;
  /** The `YYYY-MM-DD` the file was opened in. */
  readonly dateBucket: string;
  /** The zero-padded `HH` the file was opened in. */
  readonly hourBucket: string;
  /** The `now` the file was opened with. Carried for diagnostics; not consulted by `shouldRoll`. */
  readonly openedAt: Date;
}

/** Why `shouldRoll` returned other than `"no"`. Checked in this precedence order: a date or hour
 * change always wins over a size threshold, since rolling for date/hour already opens a fresh file
 * and makes the size check moot. */
export type RollReason = "no" | "size" | "date" | "hour";

function pad(n: number, width: number): string {
  return n.toString().padStart(width, "0");
}

/** The local `YYYY-MM-DD` date bucket `d` falls in: the dated directory a file opened at `d` lives in. */
export function dateBucketOf(d: Date): string {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1, 2)}-${pad(d.getDate(), 2)}`;
}

function hourBucketOf(d: Date): string {
  return pad(d.getHours(), 2);
}

/** The path the open log file should have right now, given `role`, `pid`, and a per-file `seq`. */
export function currentPath(
  policy: RotationPolicy,
  now: Date,
  role: string,
  pid: number,
  seq: number,
): string {
  if (!KNOWN_ROLES.has(role as LogRole)) {
    throw new InvalidLogRoleError({ role });
  }
  void policy; // reserved for future per-role path policy; unused today.
  const date = dateBucketOf(now);
  const hour = hourBucketOf(now);
  return `logs/${role}/${date}/events-${hour}-${pid}-${pad(seq, 3)}.jsonl`;
}

/** Whether the open file described by `state` should roll, and why. Date and hour changes take
 * precedence over the size threshold (see `RollReason`). */
export function shouldRoll(
  state: RollState,
  now: Date,
  bytesWritten: number,
  policy: RotationPolicy,
): RollReason {
  if (dateBucketOf(now) !== state.dateBucket) return "date";
  if (hourBucketOf(now) !== state.hourBucket) return "hour";
  if (bytesWritten >= policy.maxFileBytes) return "size";
  return "no";
}

/** One file on disk under `logs/<role>/<dir>/`. `dir` is the bare `YYYY-MM-DD` date bucket —
 * NOT the full `logs/<role>/<YYYY-MM-DD>` path (contrast `RollState.dir`). A malformed bucket
 * fails `plan` loudly via `InvalidDateBucketError` rather than being silently kept. */
export interface FileEntry {
  readonly path: string;
  readonly dir: string;
  readonly bytes: number;
  readonly mtime: Date;
  /** The inode, when the platform reports one: part of the identity the sweeper revalidates. */
  readonly ino?: number;
}

/** The result of `plan`: which files to compress, which to remove, and which to leave untouched.
 * Every input path appears in exactly one of the three lists. Lists are sorted for determinism. */
export interface RetentionPlan {
  readonly gzip: readonly string[];
  readonly delete: readonly string[];
  readonly keep: readonly string[];
}

const DATE_BUCKET_PATTERN = /^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$/;

/** Parse a `YYYY-MM-DD` date bucket to a local-midnight `Date`. Rejects anything that is not a
 * real calendar date in bucket shape — including full `logs/<role>/<date>` paths — with
 * `InvalidDateBucketError`, so `plan` fails loudly instead of silently keeping the entry. */
function parseDateBucket(bucket: string): Date {
  if (!DATE_BUCKET_PATTERN.test(bucket)) {
    throw new InvalidDateBucketError({ dir: bucket });
  }
  const [y, m, d] = bucket.split("-").map(Number);
  const date = new Date(y, m - 1, d);
  if (dateBucketOf(date) !== bucket) {
    // Pattern-shaped but not a real date, e.g. 2026-02-30 (`Date` overflows into March).
    throw new InvalidDateBucketError({ dir: bucket });
  }
  return date;
}

/** Whole days between dated directory `dir` and `today`, both `YYYY-MM-DD`. Computed via local
 * midnight Date objects (not a fixed millisecond divisor) so a DST transition day still counts as
 * exactly one day. */
function ageInDays(dir: string, today: string): number {
  const msPerDay = 24 * 60 * 60 * 1000;
  return Math.round((parseDateBucket(today).getTime() - parseDateBucket(dir).getTime()) / msPerDay);
}

/**
 * Compute the retention plan for `entries` as of `now`.
 *
 * Today's dated directory is always kept in full — it holds the currently-open file, which is
 * never gzipped or deleted. Non-today directories are deleted once they are `keepDays` old or more,
 * gzipped once they are `gzipAfterDays` old or more (and not yet due for deletion), and otherwise
 * kept. A path already ending in `.gz` was compressed by an earlier sweep and is gzip-ineligible:
 * it falls through to `keep`, while its bytes still count toward `maxTotalBytes` and it remains
 * eligible for age-based `delete`. If the bytes still on disk (kept + gzip-pending) exceed
 * `maxTotalBytes`, whole non-today directories are moved to `delete` oldest-first — overriding any
 * gzip decision for that directory — until the total is back under the cap or only today's
 * directory remains.
 *
 * Idempotent: re-running `plan` with the same `now` against the post-plan file set — `delete`
 * paths gone, each `gzip` source replaced in place by `<path>.gz` per §4.2's on-disk layout —
 * yields an empty `gzip` and `delete`. See `tests/unit/rotation_test.ts`'s idempotency case.
 *
 * `pinned` (item 36) names paths that must stay exactly as they are whatever their age: files the
 * sweeper could not prove safe to touch (open, unsettled, not fully ingested — see
 * `retentionBlock`). A pinned file is always in `keep`, and its bytes still count toward
 * `maxTotalBytes` because they are still on disk. The cap therefore keeps pruning *other* old files,
 * oldest directory first, until the total fits or nothing unpinned outside today is left; pinned
 * bytes that keep the total over the cap are the caller's unsafe backlog to report, never a reason
 * to touch them. With no pins this is exactly the item-06 policy.
 *
 * Throws `InvalidDateBucketError` if any entry's `dir` is not a bare `YYYY-MM-DD` date bucket.
 */
export function plan(
  entries: readonly FileEntry[],
  now: Date,
  policy: RotationPolicy,
  pinned: ReadonlySet<string> = new Set(),
): RetentionPlan {
  const today = dateBucketOf(now);

  const byDir = new Map<string, FileEntry[]>();
  for (const entry of entries) {
    const list = byDir.get(entry.dir);
    if (list) list.push(entry);
    else byDir.set(entry.dir, [entry]);
  }
  // NOTE: the pruning unit is the bare date bucket, so same-date directories across roles
  // (e.g. server/2026-10-08 and reader/2026-10-08) prune as a single unit. Whole-directory
  // pruning inherently overshoots the cap; the wider unit is within spec ("oldest dated
  // directories pruned first") but worth knowing when sizing `maxTotalBytes`.

  const gzipSet = new Set<string>();
  const deleteSet = new Set<string>();
  const keepSet = new Set<string>();

  /** Bytes the cap could still free per non-today directory (its unpinned files), oldest-first. */
  const onDiskNonToday: Array<{ dir: string; bytes: number }> = [];
  let total = 0;

  for (const [dir, list] of byDir) {
    const bytes = list.reduce((sum, e) => sum + e.bytes, 0);
    if (dir === today) {
      for (const entry of list) keepSet.add(entry.path);
      total += bytes;
      continue;
    }

    const free = list.filter((entry) => !pinned.has(entry.path));
    const held = list.filter((entry) => pinned.has(entry.path));
    for (const entry of held) keepSet.add(entry.path);
    // A pinned file stays on disk whatever happens to its directory, so it always counts.
    total += held.reduce((sum, e) => sum + e.bytes, 0);
    const freeBytes = free.reduce((sum, e) => sum + e.bytes, 0);

    const age = ageInDays(dir, today);
    if (age >= policy.keepDays) {
      for (const entry of free) deleteSet.add(entry.path);
      continue;
    }

    if (age >= policy.gzipAfterDays) {
      for (const entry of free) {
        // §4.2: already compressed by an earlier sweep — gzip-ineligible, falls through to keep.
        if (entry.path.endsWith(".gz")) keepSet.add(entry.path);
        else gzipSet.add(entry.path);
      }
    } else {
      for (const entry of free) keepSet.add(entry.path);
    }
    onDiskNonToday.push({ dir, bytes: freeBytes });
    total += freeBytes;
  }

  onDiskNonToday.sort((a, b) => (a.dir < b.dir ? -1 : a.dir > b.dir ? 1 : 0));

  for (const { dir, bytes } of onDiskNonToday) {
    if (total <= policy.maxTotalBytes) break;
    for (const entry of byDir.get(dir)!) {
      if (pinned.has(entry.path)) continue;
      gzipSet.delete(entry.path);
      keepSet.delete(entry.path);
      deleteSet.add(entry.path);
    }
    total -= bytes;
  }

  return {
    gzip: [...gzipSet].sort(),
    delete: [...deleteSet].sort(),
    keep: [...keepSet].sort(),
  };
}

// ---------------------------------------------------------------------------
// The ingest-before-retention gate (item 36, ADR 0007)
// ---------------------------------------------------------------------------

/**
 * Why a file `plan` would gzip or delete must instead be pinned. Checked in this order; the first
 * that applies is the reason:
 *
 * - `open`: a sink in the sweeping process holds it open.
 * - `unsettled`: modified less than `settleMs` before `now` (or in the future): a write that began
 *   before its hour ended may still be landing.
 * - `changed`: it is no longer the file the inventory saw (size, mtime or inode).
 * - `unreadable`: its content could not be read (or, for `.gz`, decompressed) to prove anything.
 * - `no-watermark`: ingest has never committed an offset for it.
 * - `unread-tail`: the committed offset stops short of the end — unread lines or a partial tail.
 * - `watermark-mismatch`: the committed offset is past the end, or the content is not the bytes
 *   that ingest committed (a rewrite since).
 * - `unreconciled`: the derived store and the quarantine do not account for every record exactly.
 * - `quarantine-evidence`: a `rejected.jsonl` some retained source's rejects still live in.
 */
export type RetentionBlock =
  | "open"
  | "unsettled"
  | "changed"
  | "unreadable"
  | "no-watermark"
  | "unread-tail"
  | "watermark-mismatch"
  | "unreconciled"
  | "quarantine-evidence";

/** The committed `_ingest_state` row for one source, as the gate needs it. */
export interface CommittedWatermark {
  readonly bytesIngested: number;
  /** Hex SHA-256 of bytes `[0, bytesIngested)`. */
  readonly prefixSha256: string;
  /** The source was rewritten at some point; its lines were deduplicated by `eventId`. */
  readonly dedupe: boolean;
}

/** What reading one source (decompressed, for a `.gz`) proved about its content. */
export interface SourceContent {
  readonly bytes: number;
  /** Hex SHA-256 of the whole content. */
  readonly sha256: string;
  /** `recordOffsets(content)`. */
  readonly recordOffsets: readonly number[];
}

/** Everything the gate judges one event-source file on. */
export interface RetentionFacts {
  readonly open: boolean;
  readonly mtime: Date;
  /** `false` when the file changed between inventory and the read that produced `content`. */
  readonly unchanged: boolean;
  readonly content?: SourceContent;
  readonly watermark?: CommittedWatermark;
  /** `platform_events.source_offset`s ingested from this source. Unused for a `dedupe` source. */
  readonly accepted: ReadonlySet<number>;
  /** `sourceOffset`s of this source's records in its directory's `rejected.jsonl`. */
  readonly rejected: ReadonlySet<number>;
}

/**
 * Where every record of `content` starts: each complete non-blank line, exactly as ingest cuts them
 * (`core/eventlog/offsets.completeLines`), plus the start of a trailing partial line if there is one.
 * A partial tail is only ever consumed by sealing it into quarantine (a `torn-tail` reject), so once
 * the watermark covers the whole file the tail is a record like any other.
 */
export function recordOffsets(content: Uint8Array): number[] {
  const { lines, end } = completeLines(content, 0);
  const offsets = lines.map((line) => line.start);
  if (end < content.length) offsets.push(end);
  return offsets;
}

/**
 * The gate: `undefined` if this event source may be gzipped or deleted, else why not. Pure.
 *
 * Safe means: no writer can still add to it (closed and settled), it is still the file that was
 * read, the committed ingest offset covers its entire content and vouches for exactly those bytes,
 * and — unless it was ever rewritten — every record is accounted for exactly once, as an accepted
 * row or a quarantined reject, with nothing accounted for that is not a record. A `dedupe` source's
 * lines may have landed under other sources' identities, so for it the watermark is the evidence.
 */
export function retentionBlock(
  facts: RetentionFacts,
  now: Date,
  settleMs: number,
): RetentionBlock | undefined {
  if (facts.open) return "open";
  const age = now.getTime() - facts.mtime.getTime();
  if (age < 0 || age < settleMs) return "unsettled";
  if (!facts.unchanged) return "changed";
  const content = facts.content;
  if (content === undefined) return "unreadable";
  const watermark = facts.watermark;
  if (watermark === undefined) return "no-watermark";
  if (watermark.bytesIngested < content.bytes) return "unread-tail";
  if (watermark.bytesIngested > content.bytes || watermark.prefixSha256 !== content.sha256) {
    return "watermark-mismatch";
  }
  if (watermark.dedupe) return undefined;
  const records = new Set(content.recordOffsets);
  for (const offset of records) {
    if (!facts.accepted.has(offset) && !facts.rejected.has(offset)) return "unreconciled";
  }
  for (const offset of facts.accepted) if (!records.has(offset)) return "unreconciled";
  for (const offset of facts.rejected) if (!records.has(offset)) return "unreconciled";
  return undefined;
}
