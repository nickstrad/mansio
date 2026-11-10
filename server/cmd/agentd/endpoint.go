package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/metrics"
)

// jsonEndpoints is the shape the lifecycle (control.go) and admin (admin.go) endpoints share: an optional
// identity check, a bounded number of calls in flight (more are refused 503 at once, never queued), a deadline
// per call (timeout, shortened by X-Timeout-Ms), and one JSON answer: the handler's value, or
// {"code","message"[,"log_id"]} classified by classifyControl, with Retry-After on 503. Failures are logged
// once: bugs at ERROR with a log ID (<event>.FAILED), refusals that logRefusal selects at WARN (<event>.REFUSED).
type jsonEndpoints struct {
	log        *slog.Logger
	event      string // the log event prefix: LIFECYCLE, ADMIN
	timeout    time.Duration
	slots      *slots
	authorize  func(r *http.Request) error // nil: no identity check
	logRefusal func(ce *controlError) bool
}

// slots is a pool of call slots that never queues: a call that finds none free is refused and counted.
type slots struct {
	sem  chan struct{}
	shed atomic.Int64
}

// stat is the pool for metrics.Queues.
func (s *slots) stat(queue string) metrics.QueueStat {
	return metrics.QueueStat{Queue: queue, Capacity: cap(s.sem), Rejected: map[string]int64{metrics.ReasonFull: s.shed.Load()}}
}

// handle runs fn under the identity check, a slot and the deadline, and writes its answer.
func (e *jsonEndpoints) handle(fn func(ctx context.Context, r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if e.authorize != nil {
			if err := e.authorize(r); err != nil {
				e.write(w, r, nil, err)
				return
			}
		}
		select {
		case e.slots.sem <- struct{}{}:
			defer func() { <-e.slots.sem }()
		default:
			e.slots.shed.Add(1)
			e.write(w, r, nil, &controlError{"unavailable", http.StatusServiceUnavailable, "too many calls in flight; retry"})
			return
		}
		timeout := e.timeout
		if ms, err := strconv.Atoi(r.Header.Get(front.TimeoutHeader)); err == nil && ms > 0 {
			timeout = min(timeout, time.Duration(ms)*time.Millisecond)
		}
		// The listener's write timeout is short (health answers); a fence or passivate may write back for longer.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + healthWriteTimeout))
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		out, err := fn(ctx, r)
		e.write(w, r, out, err)
	}
}

// write answers one call (Release It F2, O3).
func (e *jsonEndpoints) write(w http.ResponseWriter, r *http.Request, out any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err == nil {
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	body := map[string]string{}
	ce := classifyControl(err)
	if ce == nil {
		id := logx.NewID()
		e.log.Error("call failed (bug)", "event", e.event+".FAILED", "path", r.URL.Path, "log_id", id,
			"error", err.Error(), "chain", errs.Chain(err))
		ce, body["log_id"] = &controlError{"internal", http.StatusInternalServerError, "internal error"}, id
	} else if e.logRefusal(ce) {
		e.log.Warn("call refused", "event", e.event+".REFUSED", "path", r.URL.Path, "code", ce.code, "error", err.Error())
	}
	if ce.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(err)))
	}
	body["code"], body["message"] = ce.code, ce.msg
	w.WriteHeader(ce.status)
	_ = json.NewEncoder(w).Encode(body)
}
