-- The soak's analysis (scripts/soak/run.sh runs it with `set variable dir = '<run dir>'`). DuckDB.
-- 1. phases, 2. steady-state slopes over the clean load phase after warm-up, 3. leak check after idle,
-- 4. errors inside / outside fault windows, 5. Retry-After on retried shed/unavailable, 6. verify.
.mode box
set TimeZone = 'UTC';
create or replace table s as
  select t::timestamptz as t, proc, series, try_cast(value as double) as v
  from read_csv(getvariable('dir') || '/samples.tsv', delim = '\t', header = true, all_varchar = true);
create or replace table marks as
  select t::timestamptz as t, kind, what
  from read_csv(getvariable('dir') || '/marks.tsv', delim = '\t', header = true, all_varchar = true);
create or replace table ev as
  select t::timestamptz as t, ev, db, code, coalesce(hint_ms, 0) as hint_ms, op
  from read_json(getvariable('dir') || '/events.jsonl', format = 'newline_delimited', ignore_errors = true,
                 columns = {t: 'varchar', ev: 'varchar', db: 'varchar', code: 'varchar', hint_ms: 'bigint', op: 'varchar'});
create or replace table phases as
  select a.what as phase, a.t as t0, b.t as t1
  from marks a join marks b on a.what = b.what and a.kind = 'phase_start' and b.kind = 'phase_end';
-- Fault windows, with 30 s of grace after each end (routes and breakers settle).
create or replace table faults as
  select a.what as fault, a.t as t0, b.t + interval 30 second as t1
  from marks a join marks b on a.what = b.what and a.kind = 'fault_start' and b.kind = 'fault_end';

select '1. phases' as section;
select phase, t0, t1, round(epoch(t1) - epoch(t0)) as seconds from phases order by t0;

-- Gauges that must be flat at steady state (anything that accumulates needs a reaper that keeps up). Window: load2 after its first WARMUP_S.
create or replace table steady as
  select s.*, epoch(s.t) - epoch(p.t0) as x
  from s, phases p
  where p.phase = 'load2' and s.t >= p.t0 + interval 300 second and s.t <= p.t1
    and (s.series like 'mansio_process{%' or s.series in ('ps_rss_bytes', 'objects', 'bytes')
         or s.series like 'dir_bytes%' or s.series = 'mansio_vactor_oldest_dirty_age_seconds'
         or s.series = 'mansio_vactor{stat="active"}' or s.series = 'mansio_actors'
         or s.series = 'controld_locator_cached' or s.series like 'mansio_edge_state%');
select '1a. client view per phase (frontbench summaries)' as section;
select regexp_extract(filename, '([a-z0-9]+)\.json$', 1) as phase, requests, round(ok_per_sec) as ok_per_sec,
       round(p50_us) as p50_us, round(p99_us) as p99_us, round(p999_us) as p999_us, round(max_us) as max_us,
       round(error_rate, 5) as error_rate, moves, move_failed, codes
from read_json(getvariable('dir') || '/*.json', filename = true, union_by_name = true)
where filename similar to '.*/(load[0-9]+|idle)\.json' order by phase;

select '1b. CPU per phase (cores; a restart inside a phase undercounts that process)' as section;
select p.phase, s.proc, round((max(s.v) - min(s.v)) / nullif(epoch(max(s.t)) - epoch(min(s.t)), 0), 3) as cores
from s, phases p where s.series = 'ps_cpu_seconds' and s.t between p.t0 and p.t1 group by all order by p.phase, s.proc;

select '2. steady-state slopes (load2 after 5 min warm-up)' as section;
-- change = slope × window as a fraction of the mean; flat = |change| < 10 % or the series barely moves.
select proc, series, count(*) as n, round(avg(v), 1) as mean, round(min(v), 1) as min, round(max(v), 1) as max,
       round(regr_slope(v, x) * 3600, 2) as slope_per_hour,
       round(regr_slope(v, x) * (max(x) - min(x)) / nullif(abs(avg(v)), 0), 3) as change,
       case when abs(coalesce(regr_slope(v, x) * (max(x) - min(x)) / nullif(abs(avg(v)), 0), 0)) < 0.10 then 'flat'
            else 'SLOPE' end as verdict
from steady group by all order by verdict desc, proc, series;

select '3. leak check: levels at the start, the end of load1, the end of idle, the end of load2' as section;
create or replace table lv as
  select s.proc, s.series, s.v, s.t from s
  where s.series in ('mansio_process{stat="goroutines"}', 'mansio_process{stat="open_fds"}', 'ps_rss_bytes', 'mansio_process{stat="heap_bytes"}');
with pts as (
  select 'start' as pt, (select min(t) from s) as t union all
  select 'load1_end', t1 from phases where phase = 'load1' union all
  select 'idle_end', t1 from phases where phase = 'idle' union all
  select 'load2_end', t1 from phases where phase = 'load2'
), near as (
  select pts.pt, lv.proc, lv.series, arg_min(lv.v, abs(epoch(lv.t) - epoch(pts.t))) as v
  from pts, lv where lv.t <= pts.t + interval 20 second group by all
)
select proc, series, any_value(v) filter (where pt = 'start') as start,
       any_value(v) filter (where pt = 'load1_end') as load1_end, any_value(v) filter (where pt = 'idle_end') as idle_end,
       any_value(v) filter (where pt = 'load2_end') as load2_end
from near group by all order by proc, series;

select '4. client errors inside / outside fault windows (30 s grace)' as section;
select e.code, e.op,
       count(*) filter (where f.fault is not null) as in_fault,
       count(*) filter (where f.fault is null) as outside,
       string_agg(distinct f.fault, ',') as faults
from ev e left join faults f on e.t between f.t0 and f.t1
where e.ev = 'error' group by all order by outside desc, in_fault desc;

select '4b. errors outside fault windows, by minute' as section;
select date_trunc('minute', e.t) as minute, e.code, count(*) as n
from ev e left join faults f on e.t between f.t0 and f.t1
where e.ev = 'error' and f.fault is null group by all order by minute limit 50;

select '4c. agent / edge answers by code (counter totals at the end; agents restarted reset)' as section;
select proc, regexp_extract(series, 'code="([^"]*)"', 1) as code, max(v) as total
from s where series like 'mansio_requests_total{%' group by all having max(v) > 0 order by proc, code;

select '5. retried shed / unavailable answers without a Retry-After' as section;
select code, count(*) as retries, count(*) filter (where hint_ms = 0) as no_hint
from ev where ev = 'retry' and code in ('shed', 'unavailable') group by all;

select '5b. retries by code' as section;
select code, count(*) as n from ev where ev = 'retry' group by all order by n desc;

select '6. verify' as section;
select rows, writes, len(lost) as lost, len(duplicated) as duplicated, len(failed_found) as failed_found,
       unknown_seen, unlogged, ok
from read_json(getvariable('dir') || '/verify.json');
