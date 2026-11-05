package front

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/panicx"
)

// Paths and headers of the HTTP transport.
const (
	QueryPath     = "/v1/query"
	HealthPath    = "/healthz"
	TimeoutHeader = "X-Timeout-Ms" // the client's deadline, relative, in milliseconds
)

// NewHandler is the HTTP adapter: POST /v1/query runs the pipeline, GET /healthz answers without touching
// admission or actors (so it never queues behind queries). log receives one line per internal error (with a log
// ID that is also in the response) and debug lines for other failures.
func NewHandler(s *Server, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	h := &httpHandler{s: s, log: log}
	mux.HandleFunc(QueryPath, h.query)
	mux.Handle(HealthPath, HealthHandler(s))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.write(w, r, s.answer(Request{}, nil, &BadRequestError{Msg: "unknown path", Status: http.StatusNotFound}))
	})
	return mux
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
	s   *Server
	log *slog.Logger
}

func (h *httpHandler) query(w http.ResponseWriter, r *http.Request) {
	defer func() { // a panic outside Handle (decoding headers, encoding the answer): answer internal if we still can
		if p := panicx.From(recover()); p != nil {
			out := h.s.answer(Request{}, nil, p)
			out.Stack = p.Stack
			h.write(w, r, out)
		}
	}()
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
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.write(w, r, h.s.answer(Request{}, nil, &BadRequestError{Msg: "body too large", Status: http.StatusRequestEntityTooLarge}))
			return
		}
		h.write(w, r, h.s.answer(Request{}, nil, &BadRequestError{Msg: "cannot read body"}))
		return
	}
	in.Body = body
	h.write(w, r, h.s.Handle(r.Context(), in))
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
		hd.Set("WWW-Authenticate", `Bearer realm="ts-db agent"`)
	}
	w.WriteHeader(out.Status)
	w.Write(append(body, '\n')) // a write error means the client left; nothing to do
}
