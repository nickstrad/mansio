/**
 * The rotating JSONL file sink (plan item 11, §4.2): the only module in this repo that writes
 * event-log bytes to disk.
 *
 * It owns exactly one `Deno.FsFile`, opened in append mode, and the `RollState` that goes with it.
 * Every write first asks `core/logging/rotation.shouldRoll` whether that handle is still the right
 * one for `now`; a `"date"`/`"hour"` answer closes it and opens a file in the new dated directory,
 * a `"size"` answer bumps `seq` within the same hour bucket. Nothing here decides *when* to write
 * (that is `logger.ts`'s batching fiber) and nothing here decides what a record looks like (that is
 * `core/events.ts`'s `toJsonLine`): this module is open/append/roll/close and nothing else.
 *
 * Three rules keep the mechanics honest:
 *
 * 1. **All state transitions happen under one `Effect.Semaphore` permit.** The batching fiber and
 *    the scope's drain finalizer are two different fibers writing the same handle (the finalizer
 *    runs on the fiber that closes the scope, not on the interrupted consumer), so `append` and
 *    `close` must not interleave.
 * 2. **Failures surface in the error channel as `SinkIoError`.** They are *not* swallowed here.
 *    `logger.ts` is the layer that must never fail, and it catches these to degrade (drop + count +
 *    one stderr line a minute). Keeping the sink honest about I/O is what lets the degradation
 *    policy live in exactly one place.
 * 3. **A file, once left, is never written again** — the contract retention's closed-file rule rests
 *    on (item 36, ADR 0007). Every append first rolls if `now` has left the open file's hour bucket,
 *    a roll always opens a fresh `seq` (one past the highest on disk, `.gz` included), and a failed
 *    write abandons the handle rather than finishing its torn line. So a file dated before today,
 *    once its last in-flight write has settled, can gain no more bytes from any sink, and the
 *    ingest-then-sweep coordinator may seal its torn tail and, once ingested, gzip or delete it.
 *    Changing any of this (reopening a file to finish a line, appending under a stale bucket) would
 *    let retention remove bytes that were never ingested. Buckets are local time, so every process
 *    sharing a `LOG_DIR` must share one timezone (ADR 0007).
 *
 * `rotation.currentPath` is the single source of truth for the on-disk layout, but it hard-codes
 * the `logs/` root segment (`logs/<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl`). `LOG_DIR`
 * replaces exactly that one segment, so the date bucket, hour bucket, pid and zero-padded `seq` are
 * still formatted in one place and the two modules cannot drift.
 */
import { join } from "node:path";
import { Data, Effect } from "effect";
import {
  currentPath,
  type LogRole,
  type RollState,
  type RotationPolicy,
  shouldRoll,
} from "../core/logging/rotation.ts";

/**
 * A filesystem operation against the event log failed.
 *
 * Deliberately **not** part of `core/errors.ts`'s taxonomy: it is sink-internal and never reaches a
 * handler's error channel or an HTTP status, because `logger.ts`'s `emit` is `Effect<void, never>`
 * and converts every one of these into a counted, stderr-reported drop (plan item 11 step 3).
 */
export class SinkIoError extends Data.TaggedError("SinkIoError")<{
  readonly operation: "mkdir" | "scan" | "open" | "stat" | "write" | "close";
  readonly path: string;
  readonly cause: unknown;
}> {}

export interface FileSinkOptions {
  /** `Config.logging.dir` (`LOG_DIR`), already resolved to an absolute path by item 08. */
  readonly dir: string;
  /** This process's role. One sink instance writes one role's tree — see `FileSink`. */
  readonly role: LogRole;
  /** `Deno.pid`. Part of the filename, so two processes never share a file. */
  readonly pid: number;
  readonly policy: RotationPolicy;
}

/**
 * The open-file mechanics for one role's log tree.
 *
 * One instance writes one role's directory (`logs/server/…` or `logs/reader/…`) with this process's
 * pid in every filename: the server and the reader write concurrently and must never interleave
 * into one file (plan item 11 step 4).
 */
export interface FileSink {
  /**
   * Append `lines` (one JSONL record each, newline-free — this adds the terminators) as of `now`,
   * rolling first if `shouldRoll` says the open handle is stale. One `write` syscall per call.
   *
   * `now` is supplied by the caller rather than read from `Date.now()` so that rotation decisions
   * follow Effect's `Clock`, which is what makes the date-boundary behaviour testable under
   * `TestClock`.
   */
  append(lines: readonly string[], now: Date): Effect.Effect<void, SinkIoError>;
  /** Flush (`syncData`) and close the open handle. Idempotent: closing twice is a no-op. */
  readonly close: Effect.Effect<void, SinkIoError>;
  /** Absolute path of the open file, or `undefined` before the first append. Diagnostics and tests. */
  readonly openPath: Effect.Effect<string | undefined>;
}

/** The handle and everything `shouldRoll` needs to judge it. Mutated only under the sink's permit. */
interface OpenFile {
  readonly handle: Deno.FsFile;
  readonly path: string;
  readonly roll: RollState;
  readonly seq: number;
  /** Bytes in the file, including any that were there before this process opened it. */
  bytes: number;
}

/** Where the next record belongs, with the buckets `RollState` and the seq scan need. */
interface Target {
  readonly path: string;
  readonly dir: string;
  readonly dateBucket: string;
  readonly hourBucket: string;
}

const encoder = new TextEncoder();

/**
 * Re-roots `rotation.currentPath`'s `logs/<role>/<date>/<file>` under `LOG_DIR` and recovers the
 * date and hour buckets from it. Total for the four known roles; `currentPath` throws
 * `InvalidLogRoleError` for anything else, which `openAt` converts into a `SinkIoError`.
 */
function targetFor(options: FileSinkOptions, now: Date, seq: number): Target {
  const relative = currentPath(options.policy, now, options.role, options.pid, seq);
  const segments = relative.split("/");
  const [, , dateBucket, fileName] = segments;
  // `events-<HH>-<pid>-<seq>.jsonl` — the hour bucket is the one field the scan below needs.
  const hourBucket = fileName.split("-")[1];
  const dir = join(options.dir, options.role, dateBucket);
  return { path: join(dir, fileName), dir, dateBucket, hourBucket };
}

/**
 * The next free `seq` for this hour bucket: one past the highest already on disk, or `001` when the
 * directory is empty or absent.
 *
 * Scanning rather than counting in memory is what makes a restart safe (a second process with a
 * recycled pid, or this process restarting inside the same hour, cannot reopen and interleave into
 * a file that is already there) and makes the size roll trivial: the file just closed is the current
 * maximum, so the roll lands on `-002`.
 */
function nextSeq(target: Target, pid: number): Effect.Effect<number, SinkIoError> {
  // `\d{3,}`: `seq` is padded to three digits, not limited to them. Matching exactly three would make
  // the scan blind to `-1000` and reopen it after the next size roll, breaking rule 3.
  const pattern = new RegExp(`^events-${target.hourBucket}-${pid}-(\\d{3,})\\.jsonl(\\.gz)?$`);
  return Effect.tryPromise({
    try: async () => {
      let highest = 0;
      try {
        for await (const entry of Deno.readDir(target.dir)) {
          const match = pattern.exec(entry.name);
          if (match) highest = Math.max(highest, Number(match[1]));
        }
      } catch (cause) {
        // A missing directory is the normal first-write case, not a failure.
        if (!(cause instanceof Deno.errors.NotFound)) throw cause;
      }
      return highest + 1;
    },
    catch: (cause) => new SinkIoError({ operation: "scan", path: target.dir, cause }),
  });
}

export function makeFileSink(options: FileSinkOptions): Effect.Effect<FileSink> {
  return Effect.map(Effect.makeSemaphore(1), (permit) => {
    /**
     * The open handle. Every read and every mutation below happens inside `permit.withPermits(1)`,
     * so `append` (batching fiber) and `close` (scope finalizer) never interleave.
     */
    let open: OpenFile | undefined;

    const closeOpen: Effect.Effect<void, SinkIoError> = Effect.suspend(() => {
      const current = open;
      if (current === undefined) return Effect.void;
      // Drop the reference first: a failed close must not leave a handle that a later append would
      // try to write to, and must not make `close` non-idempotent.
      open = undefined;
      return Effect.tryPromise({
        try: async () => {
          // `write` already handed the bytes to the kernel; `syncData` makes them durable. Once per
          // file (roll or shutdown), so it is never in the per-batch path.
          try {
            await current.handle.syncData();
          } catch {
            // Best effort: an unsyncable handle still has to be closed.
          }
          current.handle.close();
        },
        catch: (cause) => new SinkIoError({ operation: "close", path: current.path, cause }),
      });
    });

    const openAt = (now: Date): Effect.Effect<void, SinkIoError> =>
      Effect.gen(function* () {
        const probe = yield* Effect.try({
          try: () => targetFor(options, now, 1),
          catch: (cause) => new SinkIoError({ operation: "open", path: options.dir, cause }),
        });
        yield* Effect.tryPromise({
          try: () => Deno.mkdir(probe.dir, { recursive: true }),
          catch: (cause) => new SinkIoError({ operation: "mkdir", path: probe.dir, cause }),
        });
        const seq = yield* nextSeq(probe, options.pid);
        const target = yield* Effect.try({
          try: () => targetFor(options, now, seq),
          catch: (cause) => new SinkIoError({ operation: "open", path: probe.dir, cause }),
        });
        const handle = yield* Effect.tryPromise({
          try: () => Deno.open(target.path, { write: true, create: true, append: true }),
          catch: (cause) => new SinkIoError({ operation: "open", path: target.path, cause }),
        });
        const size = yield* Effect.tryPromise({
          try: () => handle.stat().then((info) => info.size),
          catch: (cause) => new SinkIoError({ operation: "stat", path: target.path, cause }),
        }).pipe(
          // A handle we cannot size is a handle we cannot roll on size, so give it back.
          Effect.tapError(() => Effect.sync(() => handle.close())),
        );
        open = {
          handle,
          path: target.path,
          seq,
          bytes: size,
          roll: {
            dir: target.dir,
            dateBucket: target.dateBucket,
            hourBucket: target.hourBucket,
            openedAt: now,
          },
        };
      });

    /** Rolls if the open handle is stale for `now`, then guarantees there is an open handle. */
    const ensureOpen = (now: Date): Effect.Effect<OpenFile, SinkIoError> =>
      Effect.gen(function* () {
        const current = open;
        if (current !== undefined) {
          const reason = shouldRoll(current.roll, now, current.bytes, options.policy);
          if (reason === "no") return current;
          yield* closeOpen;
        }
        yield* openAt(now);
        // `openAt` always sets `open` on success; the cast-free read keeps that invariant explicit.
        const opened = open;
        if (opened === undefined) {
          return yield* Effect.fail(
            new SinkIoError({ operation: "open", path: options.dir, cause: "sink not opened" }),
          );
        }
        return opened;
      });

    const writeAll = (handle: Deno.FsFile, bytes: Uint8Array, path: string) =>
      Effect.tryPromise({
        try: async () => {
          let written = 0;
          while (written < bytes.byteLength) {
            const n = await handle.write(bytes.subarray(written));
            if (n <= 0) {
              // A zero write means the handle is broken; fail typed rather than looping forever.
              // Thrown inside `tryPromise` this would be a defect, so the `catch` below lets a
              // `SinkIoError` through unwrapped.
              throw new SinkIoError({
                operation: "write",
                path,
                cause: `short write: ${written}/${bytes.byteLength} bytes`,
              });
            }
            written += n;
          }
          return written;
        },
        catch: (cause) =>
          cause instanceof SinkIoError ? cause : new SinkIoError({ operation: "write", path, cause }),
      });

    const append = (lines: readonly string[], now: Date): Effect.Effect<void, SinkIoError> =>
      permit.withPermits(1)(
        Effect.gen(function* () {
          if (lines.length === 0) return;
          const current = yield* ensureOpen(now);
          const bytes = encoder.encode(`${lines.join("\n")}\n`);
          const written = yield* writeAll(current.handle, bytes, current.path).pipe(
            // A failed write may have left a partial line (ENOSPC, EINTR). Give the handle back so
            // the next append opens a fresh `seq` instead of continuing a truncated record — one
            // unparseable line at a file's tail is recoverable, a corrupted boundary is not.
            Effect.tapError(() => Effect.ignore(closeOpen)),
          );
          // Counted after the write, so a failed write cannot trip the size roll on its own.
          current.bytes += written;
        }),
      );

    return {
      append,
      close: permit.withPermits(1)(closeOpen),
      openPath: Effect.sync(() => open?.path),
    };
  });
}
