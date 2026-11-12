# Container — observability path

Redrawn on 2026-11-12 for ADR 0014: the offline event-log tool and its DuckDB file are deleted; agentd's
request log is read in place or shipped. Both formats are generated from
`diagrams/c4-container-observability.py`; numbered relationships are identical. All boxes describe landed
code.

```mermaid
flowchart TB
    subgraph platform["Mansio observability"]
        agentd["Agent<br/>[Go process]<br/>server/cmd/agentd; internal/eventlog<br/>event_log: http, handler, sql, tx, queue, backup and lifecycle records; /metrics"]
        jsonl["Request log (JSONL)<br/>[Filesystem]<br/>event_log.dir: server/date/events-*.jsonl<br/>One JSON line per record, rolled by hour and size, oldest deleted past max_total_bytes or max_age"]
        results["k6 artifacts<br/>[Filesystem]<br/>logs/k6/run-id/<br/>raw.json and summary.json"]
    end
        operator["Operator<br/>[Person]<br/>jq, duckdb CLI; or their log collector and Prometheus<br/>Reads or ships the request log; scrapes metrics"]
        k6["k6 load generator<br/>[External process]<br/>perf/*.js<br/>IR load over /v1/ops; JSON metric output"]
    agentd -->|"(1) Append records through a bounded queue that drops (counted); delete the oldest files at the limits"| jsonl
    operator -->|"(2) Read in place (jq, duckdb CLI) or tail and ship to a log store"| jsonl
    operator -->|"(3) Scrape /metrics; probe /healthz, /livez, /readyz"| agentd
    k6 -->|"(4) Raw points and summary JSON"| results
    operator -->|"(5) Read summary.json; find a k6 request in the request log by its trace id"| results
    classDef planned stroke-dasharray: 5 3
    classDef person fill:#fff,stroke-width:2px
    classDef external fill:#f4f4f4
    class operator person
    class k6 external
```

## Relationships

| # | From              | To                  | Relationship                                                                                       |
| - | ----------------- | ------------------- | -------------------------------------------------------------------------------------------------- |
| 1 | Agent             | Request log (JSONL) | Append records through a bounded queue that drops (counted); delete the oldest files at the limits |
| 2 | Operator          | Request log (JSONL) | Read in place (jq, duckdb CLI) or tail and ship to a log store                                     |
| 3 | Operator          | Agent               | Scrape /metrics; probe /healthz, /livez, /readyz                                                   |
| 4 | k6 load generator | k6 artifacts        | Raw points and summary JSON                                                                        |
| 5 | Operator          | k6 artifacts        | Read summary.json; find a k6 request in the request log by its trace id                            |

## Runtime notes

agentd writes the request log when its config has event_log (dir, level, trace_sql, buffer and the size and
age limits); server/internal/eventlog owns the record format (goldens in its testdata). Emitting never blocks
a request: a full queue drops the record, and drops, write losses and deletions are counted on /metrics
(mansio_eventlog_*). mansio ships no log collector, log store, Prometheus server or dashboard: those are the
deployment's. The offline TypeScript tool that loaded the files into DuckDB is deleted (ADR 0014);
docs/observability.md has the fields and worked jq and duckdb queries.
