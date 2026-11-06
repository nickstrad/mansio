-- Report for one sqlbench run. Run from inside the run's output directory:
--   cd results/<run> && duckdb < ../../queries/report.sql      (or: scripts/report.sh results/<run>)
-- Files: requests.csv (one row per request), samples.csv (resource samples), phases.csv, schemas.csv.

-- The typed readers live in queries/views.sql; this path assumes the run directory is results/<run>.
.read ../../queries/views.sql
CREATE OR REPLACE VIEW req AS SELECT * EXCLUDE (filename) FROM requests_csv('requests.csv');
CREATE OR REPLACE VIEW smp AS SELECT * EXCLUDE (filename) FROM samples_csv('samples.csv');
CREATE OR REPLACE VIEW ph AS SELECT * EXCLUDE (filename) FROM phases_csv('phases.csv');
CREATE OR REPLACE VIEW sch AS SELECT * EXCLUDE (filename) FROM schemas_csv('schemas.csv');
CREATE OR REPLACE VIEW db_shape AS
  SELECT db, count(*) AS tables, sum(columns) AS columns, sum(indexed_columns) AS indexes, sum(seed_rows) AS rows
  FROM sch GROUP BY db;

.print '== 1. Phases: target vs achieved, success, latency (ok requests, send→reply, ms) =='
SELECT
  p.phase, p.name, p.dbs,
  round((p.start_qps + p.end_qps) / 2) AS target_qps_avg,
  round(count(*) FILTER (WHERE r.err = 'ok') / ((p.end_ms - p.start_ms) / 1000.0)) AS ok_qps,
  round(100.0 * count(*) FILTER (WHERE r.err = 'ok') / count(*), 2) AS ok_pct,
  round(quantile_cont(r.latency_us, 0.50) FILTER (WHERE r.err = 'ok') / 1000, 3) AS p50_ms,
  round(quantile_cont(r.latency_us, 0.95) FILTER (WHERE r.err = 'ok') / 1000, 3) AS p95_ms,
  round(quantile_cont(r.latency_us, 0.99) FILTER (WHERE r.err = 'ok') / 1000, 3) AS p99_ms,
  round(quantile_cont(r.latency_us, 0.999) FILTER (WHERE r.err = 'ok') / 1000, 3) AS p999_ms,
  round(max(r.latency_us) FILTER (WHERE r.err = 'ok') / 1000, 1) AS max_ms,
  round(quantile_cont(r.e2e_us, 0.99) FILTER (WHERE r.err = 'ok') / 1000, 3) AS e2e_p99_ms
FROM ph p JOIN req r USING (phase)
GROUP BY p.phase, p.name, p.dbs, p.start_qps, p.end_qps, p.start_ms, p.end_ms ORDER BY p.phase;

.print '== 2. Outcomes per phase (count) =='
PIVOT (SELECT phase, err FROM req) ON err USING count(*) GROUP BY phase ORDER BY phase;

.print '== 3. Per workload class: latency (ok), timeouts, rows =='
SELECT
  class, any_value(write) AS write, any_value(expect_slow) AS expect_slow, count(*) AS n,
  round(100.0 * count(*) FILTER (WHERE err = 'stmt_timeout') / count(*), 2) AS timeout_pct,
  round(100.0 * count(*) FILTER (WHERE err NOT IN ('ok', 'stmt_timeout')) / count(*), 2) AS other_err_pct,
  round(quantile_cont(exec_us, 0.50) FILTER (WHERE err = 'ok') / 1000, 3) AS exec_p50_ms,
  round(quantile_cont(exec_us, 0.99) FILTER (WHERE err = 'ok') / 1000, 3) AS exec_p99_ms,
  round(quantile_cont(latency_us, 0.99) FILTER (WHERE err = 'ok') / 1000, 3) AS latency_p99_ms,
  round(avg(rows) FILTER (WHERE err = 'ok'), 1) AS avg_rows
FROM req GROUP BY class ORDER BY expect_slow, write, n DESC, class;

.print '== 4. Where the time goes (ok requests, µs): queue → exec → commit =='
SELECT
  phase, CASE WHEN write THEN 'write' ELSE 'read' END AS kind, count(*) AS n,
  round(avg(queue_us), 1) AS queue_avg, round(quantile_cont(queue_us, 0.99), 1) AS queue_p99,
  round(avg(exec_us), 1) AS exec_avg, round(quantile_cont(exec_us, 0.99), 1) AS exec_p99,
  round(avg(commit_us), 1) AS commit_avg, round(quantile_cont(commit_us, 0.99), 1) AS commit_p99
FROM req WHERE err = 'ok' GROUP BY ALL ORDER BY phase, kind;

.print '== 5. Do slow queries hurt everyone else? Normal-class p99 vs timeouts in the same second =='
WITH sec AS (
  SELECT phase, floor(send_ms / 1000)::INT AS s,
         count(*) FILTER (WHERE err = 'stmt_timeout') AS timeouts,
         quantile_cont(latency_us, 0.99) FILTER (WHERE err = 'ok' AND NOT expect_slow) AS normal_p99_us
  FROM req GROUP BY ALL
)
SELECT phase,
       CASE WHEN timeouts = 0 THEN '0' WHEN timeouts < 10 THEN '1-9' WHEN timeouts < 100 THEN '10-99' ELSE '100+' END AS timeouts_that_second,
       count(*) AS seconds, round(avg(normal_p99_us) / 1000, 3) AS normal_p99_ms
FROM sec GROUP BY ALL ORDER BY phase, timeouts_that_second;

.print '== 6. Group commit: write batch sizes =='
SELECT phase, count(*) AS writes,
       round(avg(batch), 1) AS avg_batch, quantile_disc(batch, 0.5) AS p50_batch,
       quantile_disc(batch, 0.99) AS p99_batch, max(batch) AS max_batch,
       round(quantile_cont(commit_us, 0.99) / 1000, 3) AS commit_p99_ms
FROM req WHERE write AND err = 'ok' GROUP BY phase ORDER BY phase;

.print '== 7. Fairness across databases: spread of per-database p99 (ms, dbs with >= 20 ok requests) =='
WITH per_db AS (
  SELECT phase, db, count(*) AS n, quantile_cont(latency_us, 0.99) AS p99_us
  FROM req WHERE err = 'ok' AND NOT expect_slow GROUP BY ALL HAVING count(*) >= 20
)
SELECT phase, count(*) AS dbs,
       round(min(p99_us) / 1000, 3) AS best_db_p99, round(median(p99_us) / 1000, 3) AS median_db_p99,
       round(quantile_cont(p99_us, 0.99) / 1000, 3) AS p99_db_p99, round(max(p99_us) / 1000, 3) AS worst_db_p99
FROM per_db GROUP BY phase ORDER BY phase;

.print '== 8. Hot databases: busiest 10 by requests, with their schema shape =='
SELECT r.db, count(*) AS requests, round(100.0 * count(*) / (SELECT count(*) FROM req), 2) AS pct_of_all,
       round(quantile_cont(latency_us, 0.99) FILTER (WHERE err = 'ok') / 1000, 3) AS p99_ms,
       count(*) FILTER (WHERE err = 'shed') AS shed, s.tables, s.indexes, s.rows
FROM req r JOIN db_shape s USING (db) GROUP BY r.db, s.tables, s.indexes, s.rows ORDER BY requests DESC, r.db LIMIT 10;

.print '== 9. Does schema shape matter? Scan-type exec time by database size =='
SELECT class,
       CASE WHEN s.rows < 500 THEN 'a <500' WHEN s.rows < 1000 THEN 'b 500-999' WHEN s.rows < 1500 THEN 'c 1000-1499' ELSE 'd 1500+' END AS db_rows,
       count(*) AS n, round(quantile_cont(exec_us, 0.5), 1) AS exec_p50_us, round(quantile_cont(exec_us, 0.99), 1) AS exec_p99_us
FROM req r JOIN db_shape s USING (db)
WHERE err = 'ok' AND class IN ('filter_scan', 'aggregate', 'top_n', 'range_scan')
GROUP BY ALL ORDER BY class, db_rows;

.print '== 10. Resources per phase (from samples) =='
SELECT
  phase, max(dbs) AS dbs, max(open_dbs) AS open_dbs,
  max(goroutines) AS max_goroutines, max(os_threads) AS os_threads,
  max(heap_mb) AS max_heap_mb, max(go_total_mb) AS max_go_mb, max(max_rss_mb) AS peak_rss_mb, max(fds) AS max_fds,
  round(100 * (max(user_cpu_s + sys_cpu_s) - min(user_cpu_s + sys_cpu_s)) / nullif((max(t_ms) - min(t_ms)) / 1000, 0), 0) AS cpu_pct_of_one_core,
  max(sched_p99_us) AS worst_sched_p99_us, max(gc_pause_p99_us) AS worst_gc_pause_p99_us,
  max(queue_total) AS max_queued, max(queue_max) AS max_one_mailbox, max(in_flight) AS max_in_flight,
  max(records_dropped) AS records_dropped
FROM smp GROUP BY phase ORDER BY phase;

.print '== 11. Cost per open database (end of each phase, relative to before the first database) =='
WITH base AS (SELECT * FROM smp ORDER BY t_ms LIMIT 1),
     last AS (SELECT * FROM smp s WHERE phase >= 0 QUALIFY row_number() OVER (PARTITION BY phase ORDER BY t_ms DESC) = 1)
SELECT l.phase, l.open_dbs,
       round((l.max_rss_mb - b.max_rss_mb) * 1024 / nullif(l.open_dbs, 0), 1) AS rss_kib_per_db,
       round((l.go_total_mb - b.go_total_mb) * 1024 / nullif(l.open_dbs, 0), 1) AS go_kib_per_db,
       round((l.fds - b.fds) / nullif(l.open_dbs, 0), 2) AS fds_per_db,
       round((l.goroutines - b.goroutines) / nullif(l.open_dbs, 0), 2) AS goroutines_per_db
FROM last l, base b ORDER BY l.phase;

.print '== 12. Setup cost: creating and opening databases =='
SELECT phase, name, dbs, added_dbs, seed_ms, open_ms,
       round(seed_ms / nullif(added_dbs, 0), 2) AS seed_ms_per_db, round(open_ms / nullif(added_dbs, 0), 3) AS open_ms_per_db,
       sent, gen_full
FROM ph ORDER BY phase;

.print '== 13. Timeline (per 5 s): target vs achieved, p99, errors, scheduler latency =='
WITH r AS (
  SELECT floor(send_ms / 5000)::INT * 5 AS t_s, count(*) FILTER (WHERE err = 'ok') AS ok,
         count(*) FILTER (WHERE err <> 'ok') AS errors,
         quantile_cont(latency_us, 0.99) FILTER (WHERE err = 'ok' AND NOT expect_slow) AS p99_us
  FROM req GROUP BY ALL
), s AS (
  SELECT floor(t_ms / 5000)::INT * 5 AS t_s, max(phase) AS phase, round(avg(target_qps)) AS target_qps,
         max(goroutines) AS goroutines, max(sched_p99_us) AS sched_p99_us, max(queue_total) AS queued
  FROM smp GROUP BY ALL
)
SELECT s.t_s, s.phase, s.target_qps, round(r.ok / 5.0) AS ok_qps, r.errors, round(r.p99_us / 1000, 3) AS normal_p99_ms,
       s.sched_p99_us, s.goroutines, s.queued
FROM s LEFT JOIN r USING (t_s) ORDER BY s.t_s;

.print '== 14. Retries by client profile: amplification and what the caller saw =='
WITH logical AS (
  SELECT client, count(*) FILTER (WHERE attempt = 1) AS requests, count(*) AS attempts,
         count(*) FILTER (WHERE final AND err = 'ok') AS succeeded,
         count(*) FILTER (WHERE final AND err <> 'ok') AS failed,
         count(*) FILTER (WHERE final AND err = 'ok' AND attempt > 1) AS saved_by_retry,
         quantile_cont(e2e_us, 0.50) FILTER (WHERE final AND err = 'ok') AS e2e_p50_us,
         quantile_cont(e2e_us, 0.99) FILTER (WHERE final AND err = 'ok') AS e2e_p99_us,
         max(attempt) AS max_attempt
  FROM req GROUP BY client
)
SELECT client, requests, attempts, round(attempts / requests, 2) AS amplification,
       round(100.0 * succeeded / requests, 2) AS success_pct, round(100.0 * failed / requests, 2) AS failed_pct,
       saved_by_retry, round(e2e_p50_us / 1000, 2) AS e2e_p50_ms, round(e2e_p99_us / 1000, 2) AS e2e_p99_ms, max_attempt
FROM logical ORDER BY client;

.print '== 15. Retries by phase and client: load added and outcome =='
SELECT phase, client, count(*) FILTER (WHERE attempt = 1) AS first_attempts, count(*) FILTER (WHERE attempt > 1) AS retries,
       round(count(*) / nullif(count(*) FILTER (WHERE attempt = 1), 0), 2) AS amplification,
       round(100.0 * count(*) FILTER (WHERE err = 'shed') / count(*), 2) AS shed_pct,
       round(100.0 * count(*) FILTER (WHERE final AND err = 'ok') / nullif(count(*) FILTER (WHERE attempt = 1), 0), 2) AS success_pct,
       round(quantile_cont(latency_us, 0.99) FILTER (WHERE err = 'ok' AND NOT expect_slow) / 1000, 3) AS attempt_p99_ms
FROM req GROUP BY ALL ORDER BY phase, client;

.print '== 16. Final errors after retries (what the caller finally got) =='
PIVOT (SELECT client, err FROM req WHERE final AND err <> 'ok') ON err USING count(*) GROUP BY client ORDER BY client;
