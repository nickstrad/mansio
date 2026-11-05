-- Typed readers for sqlbench's CSV files, shared by report.sql and compare.sql (SQL G-19: one definition of each
-- input, explicit columns instead of type sniffing, so a short run whose column is all empty or all 0 reads the
-- same types as a long one).
-- Each macro takes a path or glob and returns the file's columns plus `filename`.
--
-- The column lists mirror the writers: requests.csv internal/stats/writer.go (recordColumns), samples.csv
-- internal/stats/sampler.go (sampleColumns), phases.csv and schemas.csv internal/bench/plan.go (phaseColumns,
-- schemaColumns). queries/report_test.go fails when a writer's header and this file disagree.

CREATE OR REPLACE MACRO requests_csv(path) AS TABLE
  SELECT *
  FROM read_csv(path, header = true, filename = true, columns = {
    'phase': 'INTEGER', 'client': 'VARCHAR', 'attempt': 'INTEGER', 'final': 'BOOLEAN', 'db': 'INTEGER',
    'class': 'VARCHAR', 'write': 'BOOLEAN', 'expect_slow': 'BOOLEAN', 'err': 'VARCHAR', 'reopened': 'BOOLEAN',
    'batch': 'INTEGER', 'rows': 'BIGINT', 'sched_ms': 'DOUBLE', 'send_ms': 'DOUBLE', 'latency_us': 'DOUBLE',
    'e2e_us': 'DOUBLE', 'queue_us': 'DOUBLE', 'exec_us': 'DOUBLE', 'commit_us': 'DOUBLE'
  });

CREATE OR REPLACE MACRO samples_csv(path) AS TABLE
  SELECT *
  FROM read_csv(path, header = true, filename = true, columns = {
    't_ms': 'DOUBLE', 'phase': 'INTEGER', 'dbs': 'INTEGER', 'target_qps': 'DOUBLE', 'completed': 'BIGINT',
    'goroutines': 'BIGINT', 'os_threads': 'BIGINT', 'heap_mb': 'DOUBLE', 'go_total_mb': 'DOUBLE',
    'max_rss_mb': 'DOUBLE', 'fds': 'BIGINT', 'user_cpu_s': 'DOUBLE', 'sys_cpu_s': 'DOUBLE', 'gc_cycles': 'BIGINT',
    'gc_pause_p99_us': 'DOUBLE', 'sched_p50_us': 'DOUBLE', 'sched_p99_us': 'DOUBLE', 'open_dbs': 'BIGINT',
    'queue_total': 'BIGINT', 'queue_max': 'BIGINT', 'in_flight': 'BIGINT', 'records_dropped': 'BIGINT'
  });

-- quote = '': the mix column holds spaces and '=' but never quotes.
CREATE OR REPLACE MACRO phases_csv(path) AS TABLE
  SELECT *
  FROM read_csv(path, header = true, filename = true, quote = '', columns = {
    'phase': 'INTEGER', 'name': 'VARCHAR', 'dbs': 'INTEGER', 'added_dbs': 'INTEGER', 'setup_start_ms': 'DOUBLE',
    'seed_ms': 'DOUBLE', 'open_ms': 'DOUBLE', 'start_ms': 'DOUBLE', 'end_ms': 'DOUBLE', 'shape': 'VARCHAR',
    'start_qps': 'DOUBLE', 'end_qps': 'DOUBLE', 'expected_requests': 'DOUBLE', 'sent': 'BIGINT',
    'gen_full': 'BIGINT', 'distribution': 'VARCHAR', 'mix': 'VARCHAR'
  });

CREATE OR REPLACE MACRO schemas_csv(path) AS TABLE
  SELECT *
  FROM read_csv(path, header = true, filename = true, columns = {
    'db': 'INTEGER', 'table_name': 'VARCHAR', 'columns': 'INTEGER', 'indexed_columns': 'INTEGER',
    'has_parent': 'BOOLEAN', 'seed_rows': 'INTEGER', 'types': 'VARCHAR'
  });
