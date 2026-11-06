-- Report for one coldload run. Run inside the run's output directory:
--   cd results/coldload/<run> && duckdb < ../../../queries/coldload/report.sql   (or scripts/coldload/report.sh)
-- Files: activations.csv (one row per Get), levels.csv (one row per size × concurrency level).

CREATE OR REPLACE VIEW act AS SELECT * FROM read_csv('activations.csv', header = true);
CREATE OR REPLACE VIEW lvl AS SELECT * FROM read_csv('levels.csv', header = true);
-- Activations that did their own work (not callers that waited on another's activation).
CREATE OR REPLACE VIEW own AS SELECT * FROM act WHERE err = 'ok' AND total_ms > 0;

.print '== 1. Per level: throughput and time to ready (ms, activating callers) =='
SELECT
  l.level, l.kind, l.size_class, round(avg(o.bytes) / 1048576, 2) AS file_mib, l.concurrency AS c, l.n, l.errors,
  round(l.wall_ms) AS wall_ms, l.act_per_s, l.mb_per_s,
  round(quantile_cont(o.total_ms, 0.50), 2) AS ready_p50,
  round(quantile_cont(o.total_ms, 0.95), 2) AS ready_p95,
  round(quantile_cont(o.total_ms, 0.99), 2) AS ready_p99,
  round(max(o.total_ms), 2) AS ready_max
FROM lvl l LEFT JOIN own o USING (level)
GROUP BY ALL ORDER BY l.level;

.print '== 2. Where the time goes: p50 per stage (ms) and fetch share of ready time =='
SELECT
  size_class, concurrency AS c, count(*) AS n,
  round(quantile_cont(ttfb_ms, 0.5), 2) AS ttfb,
  round(quantile_cont(fetch_ms, 0.5), 2) AS fetch,
  round(quantile_cont(open_ms, 0.5), 2) AS open,
  round(quantile_cont(first_query_ms, 0.5), 2) AS first_q,
  round(quantile_cont(warm_ms, 0.5), 2) AS warm,
  round(quantile_cont(total_ms, 0.5), 2) AS ready,
  round(100 * sum(fetch_ms) / sum(total_ms), 1) AS fetch_pct,
  round(100 * sum(warm_ms) / sum(total_ms), 1) AS warm_pct,
  count(*) FILTER (WHERE local_hit) AS local_hits
FROM own GROUP BY ALL ORDER BY min(level);

.print '== 3. Single-stream fetch speed (c = 1): MB/s per file and ready time per MiB =='
SELECT
  size_class, count(*) AS n, round(avg(bytes) / 1048576, 2) AS file_mib,
  round(quantile_cont(bytes / 1e6 / (fetch_ms / 1000), 0.5), 1) AS fetch_mb_s_p50,
  round(quantile_cont(total_ms / (bytes / 1048576), 0.5), 3) AS ready_ms_per_mib_p50
FROM own WHERE concurrency = 1 AND NOT local_hit AND fetch_ms > 0 GROUP BY ALL ORDER BY file_mib;

.print '== 4. Process sanity check after each level (per-database cost belongs to sqlbench, see STATE.md) =='
SELECT
  level, size_class, n,
  round((rss_mb_after - rss_mb_before) * 1024 / n, 1) AS rss_kib_per_db,
  round((go_total_mb_after - go_total_mb_before) * 1024 / n, 1) AS go_kib_per_db,
  round((goroutines_after - goroutines_before) / n, 2) AS goroutines_per_db,
  round((fds_after - fds_before) / n, 2) AS fds_per_db,
  rss_mb_after, round(passivate_ms) AS passivate_ms
FROM lvl WHERE kind = 'cold' ORDER BY level;

.print '== 5. Duplicate callers for one cold database (want fetches = 1) =='
SELECT level, size_class, n AS callers, ok, errors, fetches, round(wall_ms, 1) AS wall_ms,
  (SELECT round(max(get_ms), 2) FROM act WHERE act.level = lvl.level) AS slowest_caller_ms
FROM lvl WHERE kind = 'dup';

.print '== 6. Data quality: rows in activations.csv vs activations asked for (dropped = recorder fell behind) =='
SELECT l.level, l.n AS asked, count(a.seq) AS rows, l.records_dropped AS dropped
FROM lvl l LEFT JOIN act a USING (level) GROUP BY ALL HAVING count(a.seq) <> l.n OR l.records_dropped > 0 ORDER BY l.level;

.print '== 7. Errors =='
SELECT level, size_class, concurrency, err, count(*) AS n FROM act WHERE err <> 'ok' GROUP BY ALL ORDER BY level;
