/**
 * The event-log CLI's composition root (`scripts/logs.ts`), from what was the Deno platform reader
 * process's root (`reader_layers.ts`) before IR-6.
 *
 * ```text
 * acquire:  Config → Logging (the CLI's `reader` sink + Logger)
 * release:  Logging (drain) → Config
 * ```
 *
 * The CLI owns the DuckDB event-log file for the length of one command, so its own records go under the
 * `reader` role (the process that owns DuckDB). The ingest-then-sweep coordinator never seals, gzips or
 * deletes a path this process's sink holds open: the sink writes one file at a time, named
 * `reader/<date>/events-<HH>-<pid>-<seq>.jsonl` and always later than every earlier one of this pid, so
 * the newest such file is the open one (`ownOpenLogPaths`).
 */
import { join } from "node:path";
import { Effect, Layer } from "effect";
import { compareSourceFiles, parseSourceFile, type SourceFile } from "../core/eventlog/offsets.ts";
import { ConfigLive } from "./config.ts";
import { LoggingReaderLive } from "./logger.ts";

/**
 * The newest `reader/*\/events-*-<pid>-*.jsonl` under `logDir`: the file this process's sink holds
 * open. Empty when the sink has not written yet or the tree is unreadable; the coordinator's
 * today/settle rails still cover that file.
 */
export function ownOpenLogPaths(logDir: string, pid: number): Effect.Effect<readonly string[]> {
  return Effect.promise(async () => {
    const mine: SourceFile[] = [];
    try {
      for await (const day of Deno.readDir(join(logDir, "reader"))) {
        if (!day.isDirectory) continue;
        for await (const file of Deno.readDir(join(logDir, "reader", day.name))) {
          const source = file.isFile ? parseSourceFile(`reader/${day.name}/${file.name}`) : undefined;
          if (source !== undefined && source.pid === pid) mine.push(source);
        }
      }
    } catch {
      return [];
    }
    const newest = mine.sort(compareSourceFiles).at(-1);
    return newest === undefined ? [] : [join(logDir, newest.path)];
  });
}

/** `Config` from the environment and the CLI's own event sink, in the order of the module comment. */
export const CliRootLive = LoggingReaderLive.pipe(Layer.provideMerge(ConfigLive));
