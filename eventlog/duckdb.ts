/** DuckDB helpers shared by the event-log ingest and analysis (from the Deno platform's `duckdb.ts`, IR-6). */
import { Effect } from "effect";

/**
 * Best-effort drain of pending DuckDB N-API finalizers before the native handles close.
 *
 * `_duckdb_data_chunk` natives are freed only by GC finalizers (the binding exposes no destroy).
 * If those finalizers run at env teardown — after the native state they release is gone — the
 * process segfaults (the `FinalizeDataChunk` use-after-free). Running `gc()` twice with a
 * macrotask between forces the finalizers to run now, while the connection and instance are still
 * open, instead of at teardown. No-op unless the V8 `gc()` is exposed (`--v8-flags=--expose-gc`).
 *
 * Callers must have already dropped their references to query results; this only forces the
 * collection. Registered as a finalizer *after* the connection acquisition, it runs *before*
 * the connection and instance close (finalizers run LIFO).
 */
export function drainDuckDB(): Effect.Effect<void> {
  return Effect.gen(function* () {
    const gc = (globalThis as { gc?: () => void }).gc;
    if (typeof gc !== "function") return;
    gc();
    // A real macrotask — not `Effect.sleep`, which hangs on a `TestClock` — lets the finalizer
    // queue drain before the second pass.
    const macrotask = () => Effect.promise(() => new Promise<void>((resolve) => setTimeout(resolve, 0)));
    yield* macrotask();
    gc();
    yield* macrotask();
  });
}
