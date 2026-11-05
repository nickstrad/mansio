-- Compare several runs side by side (e.g. compare-actor vs compare-actor-readpool).
-- Run from the repo root: duckdb < queries/compare.sql   (or: scripts/compare.sh)
-- Reads every results/*/ directory; the run name is the directory name.

CREATE OR REPLACE VIEW req AS
  SELECT regexp_extract(filename, 'results/([^/]+)/', 1) AS run, * EXCLUDE (filename)
  FROM read_csv('results/*/requests.csv', header = true, filename = true, union_by_name = true);
CREATE OR REPLACE VIEW smp AS
  SELECT regexp_extract(filename, 'results/([^/]+)/', 1) AS run, * EXCLUDE (filename)
  FROM read_csv('results/*/samples.csv', header = true, filename = true, union_by_name = true);
CREATE OR REPLACE VIEW ph AS
  SELECT regexp_extract(filename, 'results/([^/]+)/', 1) AS run, * EXCLUDE (filename)
  FROM read_csv('results/*/phases.csv', header = true, filename = true, quote = '', union_by_name = true);

.print '== Runs by phase: throughput, success and latency (ok, ms) =='
SELECT r.run, p.phase, p.name, p.dbs, round((p.start_qps + p.end_qps) / 2) AS target_qps,
       round(count(*) FILTER (WHERE r.err = 'ok') / ((p.end_ms - p.start_ms) / 1000.0)) AS ok_qps,
       round(100.0 * count(*) FILTER (WHERE r.err = 'ok') / count(*), 2) AS ok_pct,
       round(quantile_cont(r.latency_us, 0.5) FILTER (WHERE r.err = 'ok' AND NOT r.expect_slow) / 1000, 3) AS p50_ms,
       round(quantile_cont(r.latency_us, 0.99) FILTER (WHERE r.err = 'ok' AND NOT r.expect_slow) / 1000, 3) AS p99_ms,
       round(quantile_cont(r.queue_us, 0.99) FILTER (WHERE r.err = 'ok') / 1000, 3) AS queue_p99_ms
FROM ph p JOIN req r ON r.run = p.run AND r.phase = p.phase
GROUP BY r.run, p.phase, p.name, p.dbs, p.start_qps, p.end_qps, p.start_ms, p.end_ms ORDER BY p.phase, r.run;

.print '== Outcomes by run =='
PIVOT (SELECT run, err FROM req) ON err USING count(*) GROUP BY run ORDER BY run;

.print '== Resources by run =='
SELECT run, max(goroutines) AS max_goroutines, max(os_threads) AS os_threads, max(max_rss_mb) AS peak_rss_mb,
       max(fds) AS max_fds, max(sched_p99_us) AS worst_sched_p99_us,
       round(100 * (max(user_cpu_s + sys_cpu_s) - min(user_cpu_s + sys_cpu_s)) / nullif((max(t_ms) - min(t_ms)) / 1000, 0), 0) AS cpu_pct_of_one_core
FROM smp GROUP BY run ORDER BY run;
