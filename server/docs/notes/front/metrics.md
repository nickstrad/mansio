# agentd metric catalog and message codes

Every Prometheus family agentd exports, the outcome classes, the operational log event codes, and how the
`shed` Retry-After is estimated.

`GET /metrics` on the lifecycle listener (`health_listen`; without one, the query listener) serves the
Prometheus text format (0.0.4) from `internal/metrics`, a dependency-free exposition: every family pulls from
a component's own counters at scrape time. **No thresholds in code** — alert rules belong in the monitoring
config. Families marked _durable_ have adapters in `cmd/agentd/metrics.go`, tested in
`TestDependencyMetricsAndCodes`, and exist only in [durable mode](durable-mode.md), which builds the S3 store,
breakers, vactor host and owner table. Families marked _event log_ come from the agent's event log. The
backpressure queue metrics are in [queue metrics](../backpressure/queue-metrics.md).

Outcome classes (same six for every integration point, `front.OutcomeClass`): `ok`; `app_error` (the
dependency answered no: bad request, ticket, wrong owner, fenced, statement rejected or too slow, aborted);
`timeout` (no answer by the deadline, nothing changed); `refused` (shed, unavailable; nothing ran); `5xx`
(internal / transient system failure); `ambiguous` (expired write that may have committed).

## Families

| Family                                                         | Type      | Labels                                                                                                   | Source                                                                                            |
| -------------------------------------------------------------- | --------- | -------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| `mansio_requests_total`                                        | counter   | `point="front"`, `class`, `code`                                                                         | `front.Server.Stats`                                                                              |
| `mansio_request_latency_seconds`                               | histogram | `point="front"`                                                                                          | every answer; Handle is bounded by deadline + grace, so a hung request is counted at its deadline |
| `mansio_in_flight`, `mansio_in_flight_high_water`              | gauge     | `point="front"`                                                                                          |                                                                                                   |
| `mansio_admission`                                             | gauge     | `stat` = in_flight, queued, in_flight_high_water, queued_high_water, tenants, dbs                        | `front.Admission.Stats`                                                                           |
| `mansio_admission_shed_total`                                  | counter   |                                                                                                          | times a queue was full                                                                            |
| `mansio_admission_drain_per_second`                            | gauge     |                                                                                                          | completions in the last whole second (the `shed` Retry-After's rate)                              |
| `mansio_actor_queue_depth`                                     | gauge     | `stat` = total, max, p99 (across actors)                                                                 | `front.StaticHost.QueueStats`                                                                     |
| `mansio_actors`                                                | gauge     |                                                                                                          | actors served (incl. the canary)                                                                  |
| `mansio_startup_pending`, `mansio_startup_pending_age_seconds` | gauge     |                                                                                                          | databases not open yet; age of the oldest (S3)                                                    |
| `mansio_ready`                                                 | gauge     |                                                                                                          | 1 when the last readiness probe passed and is fresh                                               |
| `mansio_s3_requests_total` _durable_                           | counter   | `point="s3"`, `op`, `class`, `kind`                                                                      | `objstore.Store.Metrics` (attempts, retries included)                                             |
| `mansio_s3_retries_total`, `mansio_s3_bytes_total` _durable_   | counter   | `op`                                                                                                     |                                                                                                   |
| `mansio_s3_latency_seconds` _durable_                          | histogram | `point="s3"`, `op`                                                                                       | objstore buckets (no `_sum`)                                                                      |
| `mansio_breaker_state` _durable_                               | gauge     | `dep`, `state` (1 = current)                                                                             | `breaker.State`                                                                                   |
| `mansio_breaker_transitions_total` _durable_                   | counter   | `dep`                                                                                                    | `breaker.Transitions`                                                                             |
| `mansio_vactor_events_total` _durable_                         | counter   | `event` = activation, activation_failure, fetch, local_hit, writeback, writeback_failure, bytes_uploaded | `vactor.Stats`                                                                                    |
| `mansio_vactor_last_writeback_ok_timestamp_seconds` _durable_  | gauge     |                                                                                                          | last successful publish (0 = never)                                                               |
| `mansio_vactor` _durable_                                      | gauge     | `stat` = active, fetches_in_flight, fetch_bytes_in_flight, fetch_queued                                  | `vactor.Host.Gauges`                                                                              |
| `mansio_vactor_oldest_dirty_age_seconds` _durable_             | gauge     |                                                                                                          | last tick's RPO gauge                                                                             |
| `mansio_vactor_tick` _durable_                                 | gauge     | `stat` = took_seconds, missed, timed_out                                                                 | last `TickReport`                                                                                 |
| `mansio_owner_transitions_total` _durable_                     | counter   | `to` (state entered)                                                                                     | `owner.Config.OnChange`                                                                           |
| `mansio_eventlog_records_total` _event log_                    | counter   | `result` = written, dropped (queue full), lost (write failed)                                            | `eventlog.Log.Stats`                                                                              |
| `mansio_eventlog_queue_depth` _event log_                      | gauge     |                                                                                                          | records queued for the event log's writer                                                         |

Not implemented: a Postgres placement point (`point="placement"`) in `mansio_requests_total`.

## Message codes (one per operational event; logged on transitions, not per request)

| Code                                                                                  | Level               | When                                                          |
| ------------------------------------------------------------------------------------- | ------------------- | ------------------------------------------------------------- |
| `AGENT.SHUTDOWN_START`, `AGENT.PHASE_DONE`, `AGENT.SHUTDOWN_DONE`                     | INFO                | shutdown budget                                               |
| `AGENT.PHASE_FAILED`                                                                  | WARN                | a shutdown phase ran out or failed                            |
| `AGENT.DRAIN_END`                                                                     | INFO                | front drain finished (`forced`, `in_flight_left`)             |
| `AGENT.STARTUP_ROUND`                                                                 | INFO / WARN         | one round of opening databases (WARN while some fail)         |
| `AGENT.ALL_OPEN`                                                                      | INFO                | every database open                                           |
| `AGENT.READY` / `AGENT.NOT_READY`                                                     | INFO / WARN         | readiness transitions, with the failed checks                 |
| `FRONT.UNAVAILABLE`                                                                   | WARN, sampled 1/s   | a request answered `unavailable`                              |
| `S3.REQUEST_RETRIED`                                                                  | WARN                | objstore retried an attempt                                   |
| `<DEP>.BREAKER_OPEN`                                                                  | WARN                | e.g. `S3.BREAKER_OPEN`                                        |
| `<DEP>.BREAKER_HALF_OPEN`, `<DEP>.BREAKER_CLOSED`                                     | INFO                |                                                               |
| `VACTOR.TICK_SLOW`                                                                    | WARN                | a maintenance tick missed intervals or a job hit its deadline |
| `OWNER.CLAIMED`, `OWNER.REOPENED`, `OWNER.FENCING`, `OWNER.FENCED`, `OWNER.FORGOTTEN` | INFO                | ownership transitions                                         |
| `OWNER.SUPERSEDED`, `OWNER.CLOSED`                                                    | WARN                | a higher epoch took the key; a key closed again               |
| `OWNER.RECONCILE_DONE` / `OWNER.RECONCILE_PARTIAL`                                    | INFO / WARN         | a reconcile round                                             |
| `EVENTLOG.WRITE_FAILED` _event log_                                                   | WARN, at most 1/min | the event log cannot write (records lost, counted)            |
| `EVENTLOG.CLOSE_FAILED` _event log_                                                   | WARN                | the event log did not finish writing within 2 s at exit       |

## Shed Retry-After

An admission `shed` carries a hint: `(VM queue + 1) / completions in the last whole second`, so
`Retry-After = max(1 s floor, hint)`, capped at 60 s. With no completions measured, the floor applies.
