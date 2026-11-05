// Command edge is the stateless edge of scale-out option 4 §3.6 (control-plane plan CP-7): a TLS reverse proxy
// that verifies each query's ticket, routes it on the ticket's vm claim to that agent's query listener over TLS
// (a private CA), and relays the agent's answer. Agents stay private; only the edge faces clients. See
// internal/edge.
//
//	edge -config configs/edge/local.json [-addr-file path] [-check] [-dev]
//
// -dev allows plain HTTP on either hop: to clients (no tls section; h1 + h2c) and to agents (http:// URLs).
// Without it both hops must be TLS. The lifecycle listener (health_listen) serves /livez, /readyz (503 once
// draining) and /metrics, never behind queries.
//
// Exit codes: 0 stopped cleanly after SIGINT/SIGTERM (drained), 1 failed (message and log ID on stderr, including
// a drain cut off at drain_timeout), 2 bad usage, 70 bug, 130 interrupted before serving.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/edge"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/panicx"
)

const (
	exitOK, exitError, exitUsage, exitBug, exitInterrupted = 0, 1, 2, 70, 130
	module                                                 = "edge"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

func realMain(ctx context.Context, args []string, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("edge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "edge config (JSON)")
	addrFile := fs.String("addr-file", "", "write the client listener's address here once serving, the lifecycle listener's on a second line")
	check := fs.Bool("check", false, "validate the config, keys and certificates, then exit")
	dev := fs.Bool("dev", false, "local development: allow plain HTTP to clients (no tls section) and to agents (http:// URLs)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || *cfgPath == "" {
		fmt.Fprintln(stderr, "usage: edge -config <file> [-addr-file <file>] [-check] [-dev]")
		return exitUsage
	}
	rep := reporter{stderr: stderr}
	defer func() {
		if p := panicx.From(recover()); p != nil {
			code = rep.bug(p, p.Stack)
		}
	}()
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return rep.report(err)
	}
	cfg.Dev = *dev
	if err := cfg.checkDev(); err != nil {
		return rep.report(err)
	}
	log, closeLog, err := openLog(cfg, stderr)
	if err != nil {
		return rep.report(err)
	}
	defer closeLog()
	rep.log = log
	keys, err := loadKeys(cfg.KeysFile)
	if err != nil {
		return rep.report(err)
	}
	tr, err := cfg.agentTransport()
	if err != nil {
		return rep.report(err)
	}
	defer tr.CloseIdleConnections()
	var tlsCfg *tls.Config
	if cfg.TLS != nil {
		if tlsCfg, err = cfg.serverTLS(); err != nil {
			return rep.report(err)
		}
	}
	e, err := edge.New(edge.Config{
		Resolver:  edge.StaticRoutes(cfg.Agents),
		Gate:      edge.NewGate(keys, cfg.ClockSkew.Duration, cfg.MaxTicketTTL.Duration, cfg.TicketCache),
		Limiter:   edge.NewSourceLimiter(cfg.RateLimit.PerSecond, cfg.RateLimit.Burst, cfg.RateLimit.MaxSources),
		Transport: tr, VerifySlots: cfg.VerifySlots, AgentInFlight: cfg.AgentInFlight, Breaker: cfg.AgentBreaker.config(),
		OnBreaker:      func(vm string) func(breaker.Transition) { return logx.BreakerLog(log, "agent:"+vm) },
		DefaultTimeout: cfg.DefaultTimeout.Duration, MaxTimeout: cfg.MaxTimeout.Duration, Grace: cfg.ReplyGrace.Duration,
		BodyReadTimeout: cfg.BodyReadTimeout.Duration, MaxBody: cfg.MaxBody, Log: log,
	})
	if err != nil {
		return rep.report(err)
	}
	if *check {
		fmt.Fprintf(stderr, "config ok: %d agents (%s), %d keys, tls %v\n", len(cfg.Agents),
			strings.Join(slices.Sorted(maps.Keys(cfg.Agents)), ", "), len(keys.KIDs()), cfg.TLS != nil)
		return exitOK
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "interrupted before serving")
		return exitInterrupted
	}
	return serve(ctx, cfg, e, tlsCfg, log, *addrFile, rep, stderr)
}

// serve runs both listeners until ctx ends, then shuts down within shutdown_budget: drain (readiness fails, new
// requests are shed, in-flight ones get drain_timeout), then close the lifecycle listener.
func serve(ctx context.Context, cfg *Config, e *edge.Edge, tlsCfg *tls.Config, log *slog.Logger, addrFile string, rep reporter,
	stderr io.Writer) int {
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.Listen))
	}
	hl, err := net.Listen("tcp", cfg.HealthListen)
	if err != nil {
		ln.Close()
		return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.HealthListen))
	}
	protos := new(http.Protocols)
	protos.SetHTTP1(true)
	if tlsCfg != nil {
		protos.SetHTTP2(true)
	} else {
		protos.SetUnencryptedHTTP2(true)
	}
	// No ReadTimeout/WriteTimeout: the edge sets per-request read and write deadlines itself (body read
	// timeout; deadline + grace), as the agent does (h2 streams share a connection).
	hs := &http.Server{Handler: e, Protocols: protos, TLSConfig: tlsCfg, ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration,
		IdleTimeout: cfg.IdleTimeout.Duration, MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	reg := metrics.NewRegistry()
	e.Register(reg)
	life := lifecycleServer(cfg, e, reg)
	errc := make(chan error, 2)
	go func() {
		if tlsCfg != nil {
			errc <- hs.ServeTLS(ln, "", "")
		} else {
			errc <- hs.Serve(ln)
		}
	}()
	go func() { errc <- life.Serve(hl) }()
	if addrFile != "" {
		if err := os.WriteFile(addrFile, []byte(ln.Addr().String()+"\n"+hl.Addr().String()+"\n"), 0o644); err != nil {
			hs.Close()
			life.Close()
			return rep.report(errs.Wrap(module, err, "cannot write addr-file").With("path", addrFile))
		}
	}
	log.Info("serving", "event", "EDGE.SERVING", "addr", ln.Addr().String(), "health_addr", hl.Addr().String(),
		"agents", len(cfg.Agents), "tls", tlsCfg != nil, "dev", cfg.Dev)
	fmt.Fprintf(stderr, "edge: serving on %s\n", ln.Addr())

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	start := time.Now()
	log.Info("shutting down", "event", "EDGE.SHUTDOWN_START", "in_flight", e.InFlight(), "budget_ms", cfg.ShutdownBudget.Milliseconds())
	e.Drain()
	dctx, cancel := context.WithDeadline(context.Background(), start.Add(cfg.DrainTimeout.Duration))
	drainErr := hs.Shutdown(dctx) // stops accepting, waits for in-flight requests (bounded)
	cancel()
	if drainErr != nil {
		left := e.InFlight()
		hs.Close()
		drainErr = errs.Wrap(module, drainErr, "drain did not finish in time; %d requests were cut off (writes among them may or may not have committed)", left)
	}
	lctx, cancel := context.WithDeadline(context.Background(), start.Add(cfg.ShutdownBudget.Duration))
	if life.Shutdown(lctx) != nil {
		life.Close()
	}
	cancel()
	log.Info("shutdown done", "event", "EDGE.SHUTDOWN_DONE", "took_ms", time.Since(start).Milliseconds(), "ok", drainErr == nil)
	switch {
	case serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed):
		return rep.report(errs.Wrap(module, serveErr, "listener failed; stopped"))
	case drainErr != nil:
		return rep.report(drainErr)
	}
	fmt.Fprintln(stderr, "edge: stopped")
	return exitOK
}

// lifecycleServer serves /livez, /readyz and /metrics on their own listener (Release It B4, H2). Readiness is
// "serving and not draining": an agent's outage never pulls the edge from its load balancer (Release It C5).
func lifecycleServer(cfg *Config, e *edge.Edge, reg *metrics.Registry) *http.Server {
	mux := http.NewServeMux()
	status := func(w http.ResponseWriter, code int, s string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"status": s, "in_flight": e.InFlight()})
	}
	mux.HandleFunc(front.LivePath, func(w http.ResponseWriter, r *http.Request) { status(w, http.StatusOK, "ok") })
	mux.HandleFunc(front.ReadyPath, func(w http.ResponseWriter, r *http.Request) {
		if e.Draining() {
			status(w, http.StatusServiceUnavailable, "draining")
			return
		}
		status(w, http.StatusOK, "ready")
	})
	mux.Handle("/metrics", reg.Handler())
	return &http.Server{Handler: mux, ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration, ReadTimeout: cfg.ReadHeaderTimeout.Duration,
		WriteTimeout: 5 * time.Second, IdleTimeout: cfg.IdleTimeout.Duration, MaxHeaderBytes: 16 << 10}
}

func openLog(cfg *Config, stderr io.Writer) (*slog.Logger, func(), error) {
	level := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn}[cfg.LogLevel]
	if cfg.LogFile == "" {
		return logx.New(stderr, level), func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot create log directory").With("path", cfg.LogFile)
	}
	f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot open log file").With("path", cfg.LogFile)
	}
	return logx.New(f, level), func() { f.Close() }, nil
}

// reporter classifies an error at the top level and reports it once (as agentd and controld do): an error from
// a known module is expected (exit 1), anything else a bug (exit 70).
type reporter struct {
	log    *slog.Logger
	stderr io.Writer
}

var wellFormed = map[string]bool{module: true}

func (r reporter) report(err error) int {
	if e, ok := errs.Outermost(err); ok && wellFormed[e.Module] {
		fmt.Fprintf(r.stderr, "error: %s%s\n", e.Error(), r.record(slog.LevelWarn, "edge failed", err, errs.Stack(err)))
		return exitError
	}
	return r.bug(err, errs.Stack(err))
}

func (r reporter) bug(err error, stack string) int {
	if r.log == nil {
		fmt.Fprintf(r.stderr, "%v\n%s\nerror: something unexpected went wrong. This is a bug; please report the details above\n", err, stack)
		return exitBug
	}
	fmt.Fprintf(r.stderr, "error: something unexpected went wrong. This is a bug; please report%s\n",
		r.record(slog.LevelError, "unexpected error (bug)", err, stack))
	return exitBug
}

func (r reporter) record(level slog.Level, msg string, err error, stack string) string {
	if r.log == nil {
		return ""
	}
	id := logx.NewID()
	attrs := []slog.Attr{slog.String("log_id", id), slog.String("error", err.Error()), slog.Any("chain", errs.Chain(err))}
	if stack != "" {
		attrs = append(attrs, slog.String("stack", stack))
	}
	r.log.LogAttrs(context.Background(), level, msg, attrs...)
	return " [log ID " + id + "]"
}
