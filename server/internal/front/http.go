package front

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/ir"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/panicx"
)

// Paths and headers of the HTTP transport.
const (
	QueryPath     = "/v1/query"
	HealthPath    = "/healthz"
	TimeoutHeader = "X-Timeout-Ms" // the client's deadline, relative, in milliseconds
	TraceHeader   = "X-Trace-Id"   // the client's trace id, logged with failures (/v1/ops)
	LogIDHeader   = "X-Log-Id"     // an internal failure's log id (/v1/ops; /v1/query has it in the body)
)

// UnavailableLogPerSecond bounds the WARN lines for unavailable answers: a dependency outage fails many
// requests per second, and each line beyond this rate is only counted (in the next line's "suppressed").
const UnavailableLogPerSecond = 1

// NewHandler is the HTTP adapter: POST /v1/query runs the pipeline, POST /v1/ops the document protocol (when
// EnableOps was called before), GET /healthz answers without touching
// admission or actors (so it never queues behind queries). log receives one ERROR line per internal error (with a
// log ID that is also in the response), sampled WARN lines (event FRONT.UNAVAILABLE) for unavailable answers,
// and debug lines for other failures.
func NewHandler(s *Server, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	h := &httpHandler{s: s, log: log, unavailable: sampler{perSec: UnavailableLogPerSecond}}
	mux.HandleFunc(QueryPath, h.query)
	if s.OpsEnabled() {
		mux.HandleFunc(OpsPath, h.ops)
	}
	mux.Handle(HealthPath, HealthHandler(s))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.write(w, r, s.answer(Request{}, nil, &BadRequestError{Msg: "unknown path", Status: http.StatusNotFound}))
	})
	// Every request gets read and write deadlines first, whatever its path: http.Server has no ReadTimeout or
	// WriteTimeout because h2c streams share one connection (net/http clears them after each request). query
	// sets its write deadline itself once it knows the request's own deadline.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		rc := http.NewResponseController(w)
		rc.SetReadDeadline(now.Add(s.cfg.BodyReadTimeout)) // errors: the writer cannot (e.g. a test recorder)
		if r.URL.Path != QueryPath && r.URL.Path != OpsPath {
			rc.SetWriteDeadline(now.Add(s.cfg.WriteGrace))
		}
		mux.ServeHTTP(w, r)
	})
}

// HealthHandler answers GET /healthz: 200 while serving, 503 once draining (so load balancers stop routing).
// Serve it on a separate listener too if the main one may be saturated.
func HealthHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, code := http.StatusOK, "ok"
		if s.Draining() {
			status, code = http.StatusServiceUnavailable, "draining"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"status": code, "in_flight": s.InFlight()})
	})
}

type httpHandler struct {
	s           *Server
	log         *slog.Logger
	unavailable sampler
}

// sampler lets at most perSec events through per wall-clock second and counts the rest.
type sampler struct {
	perSec int

	mu      sync.Mutex
	window  int64 // the current second (Unix)
	n       int   // events let through in window
	dropped int   // events suppressed since the last one let through
}

// allow reports whether to log the event at now and, if so, how many were suppressed before it.
//
//	post: at most perSec calls per Unix second return true
func (s *sampler) allow(now time.Time) (ok bool, suppressed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec := now.Unix(); sec != s.window {
		s.window, s.n = sec, 0
	}
	if s.n >= s.perSec {
		s.dropped++
		return false, 0
	}
	s.n++
	suppressed, s.dropped = s.dropped, 0
	return true, suppressed
}

func (h *httpHandler) query(w http.ResponseWriter, r *http.Request) {
	defer func() { // a panic outside Handle (decoding headers, encoding the answer): answer internal if we still can
		if p := panicx.From(recover()); p != nil {
			out := h.s.answer(Request{}, nil, p)
			out.Stack = p.Stack
			h.write(w, r, out)
		}
	}()
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(h.s.cfg.BodyReadTimeout + h.s.cfg.WriteGrace)) // early refusals
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.write(w, r, h.s.answer(Request{}, nil, &BadRequestError{Msg: "use POST", Status: http.StatusMethodNotAllowed}))
		return
	}
	in := Input{Ticket: bearer(r.Header.Get("Authorization"))}
	if v := r.Header.Get(TimeoutHeader); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms <= 0 || ms > int64(time.Hour/time.Millisecond) {
			h.write(w, r, h.s.answer(Request{}, nil, &BadRequestError{Msg: TimeoutHeader + " must be a positive integer number of milliseconds"}))
			return
		}
		in.Timeout = time.Duration(ms) * time.Millisecond
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(h.s.cfg.Limits.MaxBody)))
	if err != nil {
		bad := &BadRequestError{Msg: "cannot read body"}
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			bad = &BadRequestError{Msg: "body too large", Status: http.StatusRequestEntityTooLarge}
		case errors.Is(err, os.ErrDeadlineExceeded):
			bad = &BadRequestError{Msg: "body not received in time", Status: http.StatusRequestTimeout}
		}
		h.write(w, r, h.s.answer(Request{}, nil, bad))
		return
	}
	in.Body = body
	rc.SetWriteDeadline(time.Now().Add(h.s.cfg.timeout(in.Timeout) + h.s.cfg.WriteGrace))
	h.write(w, r, h.s.Handle(r.Context(), in))
}

// ops is POST /v1/ops: the same transport rules as query (deadlines, the timeout header, bearer token), with
// the body capped at ir.MaxBodyBytes + 1 bytes so ir.DecodeBody answers RequestTooLarge without the rest being
// buffered, and answers in the protocol's JSON.
func (h *httpHandler) ops(w http.ResponseWriter, r *http.Request) {
	defer func() { // a panic outside HandleOps: answer Defect if we still can
		if p := panicx.From(recover()); p != nil {
			out := opsAnswer(nil, nil, Reply{}, p)
			out.Stack = p.Stack
			h.writeOps(w, r, out)
		}
	}()
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(h.s.cfg.BodyReadTimeout + h.s.cfg.WriteGrace)) // early refusals
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeOps(w, r, opsAnswer(nil, nil, Reply{}, &BadRequestError{Msg: "use POST", Status: http.StatusMethodNotAllowed}))
		return
	}
	in := Input{Ticket: bearer(r.Header.Get("Authorization"))}
	if v := r.Header.Get(TimeoutHeader); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms <= 0 || ms > int64(time.Hour/time.Millisecond) {
			bad := ir.SchemaErr(ir.ReasonTypeMismatch, "header:"+TimeoutHeader, "a positive integer number of milliseconds, at most 3600000", v)
			h.writeOps(w, r, opsAnswer(nil, nil, Reply{}, &docexec.Failure{Err: bad}))
			return
		}
		in.Timeout = time.Duration(ms) * time.Millisecond
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, ir.MaxBodyBytes+1))
	if err != nil {
		bad := &BadRequestError{Msg: "a body that can be read"}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			bad = &BadRequestError{Msg: "a body received in time", Status: http.StatusRequestTimeout}
		}
		h.writeOps(w, r, opsAnswer(nil, nil, Reply{}, bad))
		return
	}
	in.Body = body
	rc.SetWriteDeadline(time.Now().Add(h.s.cfg.timeout(in.Timeout) + h.s.cfg.WriteGrace))
	h.writeOps(w, r, h.s.HandleOps(r.Context(), in))
}

// writeOps logs out as write does (internal errors once, with a log ID, also sent in the X-Log-Id header so
// the body stays the spec's envelope) and sends it.
func (h *httpHandler) writeOps(w http.ResponseWriter, r *http.Request, out OpsOutput) {
	hd := w.Header()
	switch {
	case out.Code == Internal:
		id := logx.NewID()
		hd.Set(LogIDHeader, id)
		attrs := []slog.Attr{slog.String("log_id", id), slog.String("path", r.URL.Path), slog.String("tag", out.Tag),
			slog.String("request_id", out.RequestID), slog.String("trace_id", r.Header.Get(TraceHeader))}
		if out.Err != nil {
			attrs = append(attrs, slog.String("error", out.Err.Error()), slog.Any("chain", errs.Chain(out.Err)))
		}
		stack := out.Stack
		if stack == "" {
			stack = errs.Stack(out.Err)
		}
		if stack != "" {
			attrs = append(attrs, slog.String("stack", stack))
		}
		if out.Tenant != "" {
			attrs = append(attrs, slog.String("tenant", out.Tenant), slog.String("sub", out.Sub))
		}
		h.log.LogAttrs(context.Background(), slog.LevelError, "request failed (internal)", attrs...)
	case out.Code == Unavailable:
		if ok, suppressed := h.unavailable.allow(h.s.clock.Now()); ok {
			attrs := []slog.Attr{slog.String("event", "FRONT.UNAVAILABLE"), slog.String("request_id", out.RequestID),
				slog.Int("suppressed", suppressed)}
			if out.Err != nil {
				attrs = append(attrs, slog.String("error", out.Err.Error()))
			}
			h.log.LogAttrs(context.Background(), slog.LevelWarn, "request refused (dependency unavailable)", attrs...)
		}
	case out.Err != nil && h.log.Enabled(context.Background(), slog.LevelDebug):
		h.log.LogAttrs(context.Background(), slog.LevelDebug, "request refused", slog.String("tag", out.Tag),
			slog.String("error", out.Err.Error()), slog.String("request_id", out.RequestID))
	}
	hd.Set("Content-Type", "application/json")
	if out.RetryAfter > 0 {
		hd.Set("Retry-After", strconv.Itoa(out.RetryAfter))
	}
	if out.Status == http.StatusUnauthorized {
		hd.Set("WWW-Authenticate", `Bearer realm="mansio agent"`)
	}
	w.WriteHeader(out.Status)
	w.Write(append(out.Body, '\n')) // a write error means the client left; nothing to do
}

// bearer extracts the token from "Bearer <token>"; anything else is "" (ticket_invalid).
func bearer(v string) string {
	const p = "Bearer "
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return strings.TrimSpace(v[len(p):])
	}
	return ""
}

// write logs out (internal errors once, with a log ID) and sends it.
func (h *httpHandler) write(w http.ResponseWriter, r *http.Request, out Output) {
	body, err := json.Marshal(out.Resp)
	if err != nil { // e.g. a REAL that is ±Inf or NaN: JSON cannot carry it
		tenant, sub := out.Tenant, out.Sub
		out = h.s.answer(Request{RequestID: out.Resp.RequestID}, nil, errs.Wrap("front", err, "cannot encode the result as JSON"))
		out.Tenant, out.Sub = tenant, sub
	}
	if out.Resp.Code == Internal {
		out.Resp.LogID = logx.NewID()
		body, _ = json.Marshal(out.Resp) // cannot fail: internal answers carry no rows
		attrs := []slog.Attr{slog.String("log_id", out.Resp.LogID), slog.String("path", r.URL.Path),
			slog.String("request_id", out.Resp.RequestID)}
		if out.Err != nil {
			attrs = append(attrs, slog.String("error", out.Err.Error()), slog.Any("chain", errs.Chain(out.Err)))
		}
		stack := out.Stack
		if stack == "" {
			stack = errs.Stack(out.Err)
		}
		if stack != "" {
			attrs = append(attrs, slog.String("stack", stack))
		}
		if out.Tenant != "" {
			attrs = append(attrs, slog.String("tenant", out.Tenant), slog.String("sub", out.Sub))
		}
		h.log.LogAttrs(context.Background(), slog.LevelError, "request failed (internal)", attrs...)
	} else if out.Resp.Code == Unavailable {
		if ok, suppressed := h.unavailable.allow(h.s.clock.Now()); ok {
			attrs := []slog.Attr{slog.String("event", "FRONT.UNAVAILABLE"), slog.String("request_id", out.Resp.RequestID),
				slog.Int("suppressed", suppressed)}
			if out.Err != nil {
				attrs = append(attrs, slog.String("error", out.Err.Error()))
			}
			if out.Tenant != "" {
				attrs = append(attrs, slog.String("tenant", out.Tenant))
			}
			h.log.LogAttrs(context.Background(), slog.LevelWarn, "request refused (dependency unavailable)", attrs...)
		}
	} else if out.Err != nil && h.log.Enabled(context.Background(), slog.LevelDebug) {
		h.log.LogAttrs(context.Background(), slog.LevelDebug, "request refused", slog.String("code", string(out.Resp.Code)),
			slog.String("error", out.Err.Error()), slog.String("request_id", out.Resp.RequestID))
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	if out.RetryAfter > 0 {
		hd.Set("Retry-After", strconv.Itoa(out.RetryAfter))
	}
	if out.Status == http.StatusUnauthorized {
		hd.Set("WWW-Authenticate", `Bearer realm="mansio agent"`)
	}
	w.WriteHeader(out.Status)
	w.Write(append(body, '\n')) // a write error means the client left; nothing to do
}
