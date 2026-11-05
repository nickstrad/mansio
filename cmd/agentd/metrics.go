package main

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// The metric catalog (Release It O1, O2) is documented in knowledge/front/2026-11-05-metrics.md. Every
// family pulls from a component's own counters at scrape time; thresholds and alerts live in the monitoring
// config, never here. Integration points share one family per kind, labeled point="front|s3|...", and the
// six outcome classes of front.OutcomeClass.

// metricsPath serves the registry on the lifecycle listener.
const metricsPath = "/metrics"

// frontMetrics exposes the request pipeline and its admission (point="front"). One snapshot per scrape: the
// admission lock is the request path's.
func frontMetrics(reg *metrics.Registry, srv *front.Server) {
	var st front.ServerStats
	var adm front.AdmissionStats
	reg.OnScrape(func() { st, adm = srv.Stats(), srv.Admission().Stats() })
	reg.Add("tsdb_requests_total", "Answers per integration point by outcome class and wire code.", metrics.Counter, func(e *metrics.Emit) {
		for i, c := range front.Codes {
			n := st.Outcomes[i]
			if front.OutcomeClass(c, true) == front.OutcomeClass(c, false) {
				n = [2]int64{n[0] + n[1], 0}
			} else {
				e.Value(metrics.Labels{"point", "front", "class", front.OutcomeClass(c, true), "code", string(c)}, float64(n[1]))
			}
			e.Value(metrics.Labels{"point", "front", "class", front.OutcomeClass(c, false), "code", string(c)}, float64(n[0]))
		}
	})
	reg.Add("tsdb_request_latency_seconds", "Answer latency per integration point (requests that hang are counted at their deadline).",
		metrics.Histogram, func(e *metrics.Emit) {
			e.Histogram(metrics.Labels{"point", "front"}, st.LatencyBounds, st.LatencyCounts, st.LatencySum)
		})
	reg.Add("tsdb_in_flight", "Requests in flight per integration point.", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(metrics.Labels{"point", "front"}, float64(st.InFlight))
	})
	reg.Add("tsdb_in_flight_high_water", "Most requests in flight at once since start, per integration point.", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(metrics.Labels{"point", "front"}, float64(st.InFlightHighWater))
	})
	reg.Add("tsdb_admission", "Admission (VM-wide): in use, queued, their high-water marks, tenants and databases with state.",
		metrics.Gauge, func(e *metrics.Emit) {
			for _, v := range []struct {
				stat string
				n    int
			}{{"in_flight", adm.InFlight}, {"queued", adm.Queued}, {"in_flight_high_water", adm.InFlightHighWater},
				{"queued_high_water", adm.QueuedHighWater}, {"tenants", adm.Tenants}, {"dbs", adm.DBs}} {
				e.Value(metrics.Labels{"stat", v.stat}, float64(v.n))
			}
		})
	reg.Add("tsdb_admission_shed_total", "Requests admission refused because a queue was full (times full).", metrics.Counter, func(e *metrics.Emit) {
		e.Value(nil, float64(adm.Shed))
	})
	reg.Add("tsdb_admission_drain_per_second", "Admission completions in the last whole second (the shed Retry-After's rate).",
		metrics.Gauge, func(e *metrics.Emit) { e.Value(nil, adm.DrainPerSec) })
}

// actorMetrics exposes the actors' queue depths (one scan of the actors per scrape).
func actorMetrics(reg *metrics.Registry, host *front.StaticHost) {
	var q front.QueueStats
	reg.OnScrape(func() { q = host.QueueStats() })
	reg.Add("tsdb_actor_queue_depth", "Requests queued in actors: total, deepest, and the p99 across actors.", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(metrics.Labels{"stat", "total"}, float64(q.Total))
		e.Value(metrics.Labels{"stat", "max"}, float64(q.Max))
		e.Value(metrics.Labels{"stat", "p99"}, float64(q.P99))
	})
	reg.Add("tsdb_actors", "Actors served (databases open, plus the canary).", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, float64(q.Actors))
	})
}

// lifecycleMetrics exposes startup and readiness (Release It S3: the oldest unfinished thing). pending counts
// the databases not served yet since start: unopened (static mode) or unreconciled (durable mode).
func lifecycleMetrics(reg *metrics.Registry, pending func() int, ready *front.Readiness, started time.Time) {
	reg.Add("tsdb_startup_pending", "Databases not open or reconciled yet (refused unavailable until they are).", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, float64(pending()))
	})
	reg.Add("tsdb_startup_pending_age_seconds", "How long the oldest unopened or unreconciled database has waited (0 when none).", metrics.Gauge, func(e *metrics.Emit) {
		age := 0.0
		if pending() > 0 {
			age = time.Since(started).Seconds()
		}
		e.Value(nil, age)
	})
	reg.Add("tsdb_ready", "1 when the last readiness probe passed and is fresh.", metrics.Gauge, func(e *metrics.Emit) {
		v := 0.0
		if ready.State().Ready {
			v = 1
		}
		e.Value(nil, v)
	})
}

// s3Class maps an objstore error kind to an outcome class. CP-1 wiring point: s3Metrics(reg, store.Metrics).
func s3Class(k objstore.Kind) string {
	switch k {
	case objstore.OK:
		return front.ClassOK
	case objstore.NotFound, objstore.Precondition, objstore.Permanent:
		return front.ClassAppError
	case objstore.Canceled:
		return front.ClassTimeout
	}
	return front.Class5xx // transient: 5xx, throttling, connection errors and our own timeouts
}

// s3Metrics exposes objstore's per-operation counters (point="s3"; one series per op and kind), from one
// snapshot per scrape.
func s3Metrics(reg *metrics.Registry, snapshot func() map[string]objstore.OpStats) {
	var ops []string
	var snap map[string]objstore.OpStats
	reg.OnScrape(func() { snap = snapshot(); ops = slices.Sorted(maps.Keys(snap)) })
	family := func(name, help string, typ metrics.Type, fn func(e *metrics.Emit, op string, st objstore.OpStats)) {
		reg.Add(name, help, typ, func(e *metrics.Emit) {
			for _, op := range ops {
				fn(e, op, snap[op])
			}
		})
	}
	family("tsdb_s3_requests_total", "S3 attempts (retries included) by operation, outcome class and kind.", metrics.Counter,
		func(e *metrics.Emit, op string, st objstore.OpStats) {
			for k := objstore.OK; k <= objstore.Canceled; k++ {
				e.Value(metrics.Labels{"point", "s3", "op", op, "class", s3Class(k), "kind", k.String()}, float64(st.ByKind[k]))
			}
		})
	family("tsdb_s3_retries_total", "Failed S3 attempts the retry layer followed with another attempt.", metrics.Counter,
		func(e *metrics.Emit, op string, st objstore.OpStats) {
			e.Value(metrics.Labels{"op", op}, float64(st.Retried))
		})
	family("tsdb_s3_bytes_total", "Body bytes moved by successful S3 attempts.", metrics.Counter,
		func(e *metrics.Emit, op string, st objstore.OpStats) {
			e.Value(metrics.Labels{"op", op}, float64(st.Bytes))
		})
	family("tsdb_s3_latency_seconds", "S3 attempt latency by operation (_sum is 0: objstore keeps buckets only).", metrics.Histogram,
		func(e *metrics.Emit, op string, st objstore.OpStats) {
			e.Histogram(metrics.Labels{"point", "s3", "op", op}, objstore.LatencyBuckets, toUint(st.Latency), 0)
		})
}

func toUint(c []int64) []uint64 {
	out := make([]uint64, len(c))
	for i, n := range c {
		out[i] = uint64(n)
	}
	return out
}

// breakerMetrics exposes a breaker's state (one series per state, 1 for the current one) and its transitions.
// CP-1 wiring point: one per remote dependency, with breakerLog as its Config.OnChange.
func breakerMetrics(reg *metrics.Registry, deps map[string]*breaker.Breaker) {
	reg.Add("tsdb_breaker_state", "Circuit breaker state per dependency (1 = current).", metrics.Gauge, func(e *metrics.Emit) {
		now := time.Now()
		for _, dep := range slices.Sorted(maps.Keys(deps)) {
			b := deps[dep]
			cur := b.State(now)
			for _, s := range []breaker.State{breaker.Closed, breaker.Open, breaker.HalfOpen} {
				v := 0.0
				if s == cur {
					v = 1
				}
				e.Value(metrics.Labels{"dep", dep, "state", s.String()}, v)
			}
		}
	})
	reg.Add("tsdb_breaker_transitions_total", "Circuit breaker state changes per dependency.", metrics.Counter, func(e *metrics.Emit) {
		for _, dep := range slices.Sorted(maps.Keys(deps)) {
			b := deps[dep]
			e.Value(metrics.Labels{"dep", dep}, float64(b.Transitions()))
		}
	})
}

// breakerLog logs every transition of dep's breaker with a stable message code: <DEP>.BREAKER_OPEN (WARN),
// <DEP>.BREAKER_HALF_OPEN and <DEP>.BREAKER_CLOSED (INFO) — e.g. S3.BREAKER_OPEN (Release It C4, O3).
func breakerLog(log *slog.Logger, dep string) func(breaker.Transition) {
	return func(t breaker.Transition) {
		code := strings.ToUpper(dep) + ".BREAKER_" + strings.ToUpper(t.To.String())
		level := slog.LevelInfo
		if t.To == breaker.Open {
			level = slog.LevelWarn
		}
		log.Log(context.Background(), level, "circuit breaker "+t.To.String(), "event", code, "dep", dep, "from", t.From.String(),
			"calls", t.Calls, "failures", t.Failures)
	}
}

// vactorMetrics exposes a virtual-actor host: Stats counters, Gauges, and the last maintenance tick (its
// oldest_dirty_age is the RPO gauge, Release It S3). CP-1 wiring point, with tickLog as the tick reporter.
func vactorMetrics(reg *metrics.Registry, h *vactor.Host, lastTick func() vactor.TickReport) {
	var st vactor.Stats
	var tick vactor.TickReport
	reg.OnScrape(func() { st, tick = h.Stats(), lastTick() })
	reg.Add("tsdb_vactor_events_total", "Virtual-actor host counters since start.", metrics.Counter, func(e *metrics.Emit) {
		for _, v := range []struct {
			event string
			n     int64
		}{{"activation", st.Activations}, {"activation_failure", st.Failures}, {"fetch", st.Fetches}, {"local_hit", st.LocalHits},
			{"writeback", st.WriteBacks}, {"writeback_failure", st.WriteBackFailures}, {"bytes_uploaded", st.BytesUploaded}} {
			e.Value(metrics.Labels{"event", v.event}, float64(v.n))
		}
	})
	reg.Add("tsdb_vactor_last_writeback_ok_timestamp_seconds", "When a write-back last published (0 = never).", metrics.Gauge,
		func(e *metrics.Emit) {
			v := 0.0
			if t := st.LastWriteBackOK; !t.IsZero() {
				v = float64(t.UnixNano()) / 1e9
			}
			e.Value(nil, v)
		})
	reg.Add("tsdb_vactor", "Virtual-actor host levels: active databases and the fetch bulkhead.", metrics.Gauge, func(e *metrics.Emit) {
		g := h.Gauges()
		e.Value(metrics.Labels{"stat", "active"}, float64(g.Active))
		e.Value(metrics.Labels{"stat", "fetches_in_flight"}, float64(g.FetchesInFlight))
		e.Value(metrics.Labels{"stat", "fetch_bytes_in_flight"}, float64(g.FetchBytesInFlight))
		e.Value(metrics.Labels{"stat", "fetch_queued"}, float64(g.FetchQueued))
	})
	reg.Add("tsdb_vactor_oldest_dirty_age_seconds", "Age of the oldest unpublished writes at the last tick (the RPO).", metrics.Gauge,
		func(e *metrics.Emit) { e.Value(nil, tick.OldestDirtyAge.Seconds()) })
	reg.Add("tsdb_vactor_tick", "The last maintenance tick: duration, intervals missed, jobs that hit their deadline.", metrics.Gauge,
		func(e *metrics.Emit) {
			e.Value(metrics.Labels{"stat", "took_seconds"}, tick.Took.Seconds())
			e.Value(metrics.Labels{"stat", "missed"}, float64(tick.Missed))
			e.Value(metrics.Labels{"stat", "timed_out"}, float64(len(tick.TimedOut)))
		})
}

// tickLog keeps the last tick for vactorMetrics and logs only a stuck or slow one (VACTOR.TICK_SLOW, WARN):
// a healthy tick is a metric, not a log line.
type tickLog struct {
	log  *slog.Logger
	mu   sync.Mutex
	last vactor.TickReport
}

func (t *tickLog) report(r vactor.TickReport) {
	t.mu.Lock()
	t.last = r
	t.mu.Unlock()
	if r.Missed > 0 || len(r.TimedOut) > 0 {
		t.log.Warn("maintenance tick slow", "event", "VACTOR.TICK_SLOW", "took_ms", r.Took.Milliseconds(), "missed", r.Missed,
			"timed_out", r.TimedOut, "oldest_dirty_age_ms", r.OldestDirtyAge.Milliseconds())
	}
}

func (t *tickLog) lastTick() vactor.TickReport {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

// ownerEvents is owner.Config.OnChange for the agent: one log line per ownership transition with a stable
// message code, and counters by new state. CP-1 wiring point.
type ownerEvents struct {
	log *slog.Logger
	to  [5]atomic.Int64 // by owner.State
}

// ownerCode names an ownership transition (Release It O3).
func ownerCode(from, to owner.Local) (code string, level slog.Level) {
	switch to.State {
	case owner.Active:
		if from.State == owner.Closed {
			return "OWNER.REOPENED", slog.LevelInfo
		}
		return "OWNER.CLAIMED", slog.LevelInfo
	case owner.Fencing:
		return "OWNER.FENCING", slog.LevelInfo
	case owner.Fenced:
		if to.Superseded {
			return "OWNER.SUPERSEDED", slog.LevelWarn
		}
		return "OWNER.FENCED", slog.LevelInfo
	case owner.Closed:
		return "OWNER.CLOSED", slog.LevelWarn
	}
	return "OWNER.FORGOTTEN", slog.LevelInfo
}

func (o *ownerEvents) onChange(key string, from, to owner.Local) {
	if int(to.State) < len(o.to) {
		o.to[to.State].Add(1)
	}
	code, level := ownerCode(from, to)
	o.log.Log(context.Background(), level, "ownership "+to.State.String(), "event", code, "db", key, "epoch", uint64(to.Epoch),
		"from", from.String(), "to", to.String())
}

func (o *ownerEvents) register(reg *metrics.Registry) {
	reg.Add("tsdb_owner_transitions_total", "Ownership transitions by the state entered.", metrics.Counter, func(e *metrics.Emit) {
		for s := owner.Absent; int(s) < len(o.to); s++ {
			e.Value(metrics.Labels{"to", s.String()}, float64(o.to[s].Load()))
		}
	})
}

// reconcileLog logs each reconcile round: OWNER.RECONCILE_DONE (INFO) once nothing stays closed,
// OWNER.RECONCILE_PARTIAL (WARN) otherwise. For owner.Table.ReconcileUntilDone at CP-1.
func reconcileLog(log *slog.Logger) func(owner.ReconcileReport, error) {
	return func(r owner.ReconcileReport, err error) {
		attrs := []any{"round", r.Round, "tried", r.Tried, "failed", r.Failed, "took_ms", r.Took.Milliseconds()}
		if err != nil {
			log.Warn("reconcile round left keys closed", append([]any{"event", "OWNER.RECONCILE_PARTIAL", "error", err.Error()}, attrs...)...)
			return
		}
		log.Info("reconcile done", append([]any{"event", "OWNER.RECONCILE_DONE"}, attrs...)...)
	}
}
