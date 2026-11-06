package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/eventlog"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/metrics"
)

// The event log (IR-4c; docs/observability.md): agentd writes mansio's event envelope as JSONL under
// event_log.dir with role "server", so `deno task logs:ingest` / `logs:query` (LOG_DIR = event_log.dir) and the
// analysis catalog work on it unchanged. Per /v1/ops request:
//
//	http     every answer: method, path, handler (X-Mansio-Handler, else "v1.ops"), status, sizes, the tag
//	handler  a decoded request: its kind, the handler name, the pipeline's time (the server's half of the
//	         SDK's handler run: the reads or the commit)
//	tx       a transaction that ran: commit / rollback / interrupted, statements, queue wait
//	sql      each statement with trace_sql, and every failed one
//
// all under the request's trace id; handler, tx and sql share one span. Plus lifecycle startup and shutdown.
// Emitting never blocks a request (eventlog: bounded queue, drops counted and reported).

// checkEventLog validates the event_log section.
func checkEventLog(e *EventLogConfig, bad func(string, ...any)) {
	if e.Dir == "" {
		bad("event_log.dir is required (LOG_DIR for deno task logs:ingest)")
	}
	if e.Level == "" {
		e.Level = "info"
	}
	if _, ok := eventlog.ParseLevel(e.Level); !ok {
		bad("event_log.level %q: want debug, info, warn or error", e.Level)
	}
	if e.Buffer < 0 || e.MaxFileBytes < 0 {
		bad("event_log: buffer and max_file_bytes must be >= 0 (0 = default)")
	}
}

// openEventLog opens cfg's event log; nil when it has none.
func openEventLog(cfg *Config, log *slog.Logger) (*eventlog.Log, error) {
	e := cfg.EventLog
	if e == nil {
		return nil, nil
	}
	level, _ := eventlog.ParseLevel(e.Level)
	ev, err := eventlog.Open(eventlog.Config{Dir: e.Dir, Role: "server", Level: level, Buffer: e.Buffer, MaxFileBytes: e.MaxFileBytes, Log: log})
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open the event log")
	}
	return ev, nil
}

// closeEventLog writes the shutdown record and what is still queued, within budget.
func closeEventLog(ev *eventlog.Log, budget time.Duration) error {
	if ev == nil {
		return nil
	}
	ev.Emit(eventlog.Event{TraceID: lifecycleTrace, Payload: eventlog.Lifecycle{Phase: "shutdown"}})
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return ev.Close(ctx)
}

// lifecycleTrace is the trace id of lifecycle records (platform/server.ts names its process trace the same way).
const lifecycleTrace = "lifecycle"

// opsObserver maps each observed /v1/ops request to its records.
func opsObserver(ev *eventlog.Log, traceSQL bool) front.OpsObserver {
	return func(o front.OpsEvent) {
		for _, rec := range opsRecords(o, traceSQL, eventlog.NewID) {
			ev.Emit(rec)
		}
	}
}

// opsRecords is the records of one request (newID draws span ids).
func opsRecords(o front.OpsEvent, traceSQL bool, newID func() string) []eventlog.Event {
	out := o.Out
	kind := string(o.Ops.Kind)
	if kind == "" {
		kind = "mutation" // undecoded: a POST, as the TS router classifies an unmatched route
	}
	var tag string
	var detail any
	if out.Status >= 400 {
		tag = out.Tag
		var body struct {
			Error map[string]any `json:"error"`
		}
		if json.Unmarshal(out.Body, &body) == nil && body.Error != nil {
			detail = body.Error // what the client was told; the cause stays in the operational log
		}
	}
	end := o.Start.Add(o.Duration)
	recs := []eventlog.Event{{Level: levelOf(out.Status), Time: end, TraceID: o.TraceID, AppID: o.Ops.App,
		Payload: eventlog.HTTP{Method: o.Method, Path: o.Path, HandlerKind: kind, Name: o.Handler, Status: out.Status,
			Duration: o.Duration, BytesIn: int64(o.BytesIn), BytesOut: int64(o.BytesOut), ErrorTag: tag, ErrorDetail: detail}}}
	if o.Ops.Kind == "" {
		return recs
	}
	span := newID()
	at := func(level eventlog.Level, t time.Time, p eventlog.Payload) eventlog.Event {
		return eventlog.Event{Level: level, Time: t, TraceID: o.TraceID, SpanID: span, AppID: o.Ops.App, Payload: p}
	}
	if tx := o.Ops; !tx.Began.IsZero() {
		outcome, level, txTag := "commit", eventlog.Info, ""
		switch {
		case out.Code == front.StmtTimeout || out.Code == front.Expired:
			outcome, level, txTag = "interrupted", eventlog.Warn, tag
		case out.Status >= 400:
			outcome, level, txTag = "rollback", levelOf(out.Status), tag
		}
		for _, st := range tx.Statements {
			if !traceSQL && st.Err == nil {
				continue
			}
			sql := eventlog.SQL{Engine: "sqlite", Dialect: "sqlite", StatementKind: statementKind(st.SQL), TableName: tableName(st.SQL),
				SQLText: st.SQL, BindCount: st.Binds, Duration: st.Duration, RowCount: st.Rows, Outcome: "ok"}
			level := eventlog.Info
			if st.Err != nil {
				sql.Outcome, sql.RowCount, sql.ErrorTag, level = "error", -1, "StorageError", eventlog.Warn
				if errors.Is(st.Err, front.ErrStmtTimeout) || out.Code == front.StmtTimeout {
					sql.ErrorTag = "StatementDeadlineError"
				}
			}
			recs = append(recs, at(level, st.Start.Add(st.Duration), sql))
		}
		recs = append(recs, at(level, tx.Ended, eventlog.Tx{Engine: "sqlite", Outcome: outcome, Operations: len(tx.Statements),
			Duration: tx.Ended.Sub(tx.Began), QueueWait: tx.Began.Sub(tx.Submitted), ErrorTag: txTag}))
	}
	outcome := "ok"
	if out.Status >= 400 {
		outcome = "error"
	}
	return append(recs, at(levelOf(out.Status), o.Start.Add(o.Ops.Pipeline), eventlog.Handler{HandlerKind: kind, Name: o.Handler,
		Outcome: outcome, Duration: o.Ops.Pipeline, ErrorTag: tag}))
}

// levelOf is platform/http/router.ts levelOf.
func levelOf(status int) eventlog.Level {
	switch {
	case status >= 500:
		return eventlog.Error
	case status >= 400:
		return eventlog.Warn
	}
	return eventlog.Info
}

// statementKind classifies a compiled statement (sql_events.statement_kind).
func statementKind(sql string) string {
	word, _, _ := strings.Cut(strings.TrimSpace(sql), " ")
	switch w := strings.ToLower(word); w {
	case "select", "insert", "update", "delete", "pragma":
		return w
	case "with":
		return "select"
	case "replace":
		return "insert"
	}
	return "ddl"
}

// tableRef finds the statement's table: the first FROM, INTO, UPDATE or TABLE target.
var tableRef = regexp.MustCompile(`(?i)\b(?:from|into|update|table(?:\s+if\s+not\s+exists)?)\s+"?([A-Za-z_][A-Za-z0-9_]*)"?`)

func tableName(sql string) string {
	if m := tableRef.FindStringSubmatch(sql); m != nil {
		return m[1]
	}
	return ""
}

// eventLogMetrics exposes the event log's counters (point="eventlog"): written, dropped (queue full: the request
// path never waits) and lost (a write failed), and the queue depth.
func eventLogMetrics(reg *metrics.Registry, ev *eventlog.Log) {
	if ev == nil {
		return
	}
	var st eventlog.Stats
	reg.OnScrape(func() { st = ev.Stats() })
	reg.Add("mansio_eventlog_records_total", "Event log records by result: written, dropped (queue full), lost (write failed).", metrics.Counter,
		func(e *metrics.Emit) {
			e.Value(metrics.Labels{"result", "written"}, float64(st.Written))
			e.Value(metrics.Labels{"result", "dropped"}, float64(st.Dropped))
			e.Value(metrics.Labels{"result", "lost"}, float64(st.Lost))
		})
	reg.Add("mansio_eventlog_queue_depth", "Event log records queued for the writer.", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, float64(st.Depth))
	})
}
