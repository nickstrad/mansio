// Command agentd is the VM agent's client-facing front (scale-out option 4 §2): it serves the SQLite databases
// in one directory, one actor per database, behind ticket verification, admission and the wire codes of
// internal/front. Requests use the INTERNAL, TRUSTED-CALLER protocol (parameterized SQL); see package front.
//
//	agentd -config configs/agentd/local.json [-addr-file path] [-check]
//
// Exit codes: 0 stopped cleanly after SIGINT/SIGTERM (drained), 1 failed (message and log ID on stderr; this
// includes a shutdown that did not finish cleanly: a drain cut off at drain_timeout, or databases not closed within
// shutdown_budget), 2 bad usage, 70 bug (report the log ID),
// 130 interrupted before it started serving.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/panicx"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

const (
	exitOK, exitError, exitUsage, exitBug, exitInterrupted = 0, 1, 2, 70, 130
	module                                                 = "agentd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// realMain runs agentd until ctx ends and returns the exit code. Every error is classified and logged once, here.
func realMain(ctx context.Context, args []string, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("agentd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "agent config (JSON)")
	addrFile := fs.String("addr-file", "", "write the query listener's address here once serving (and the lifecycle listener's on a second line)")
	check := fs.Bool("check", false, "validate the config, keys and databases, then exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || *cfgPath == "" {
		fmt.Fprintln(stderr, "usage: agentd -config <file> [-addr-file <file>] [-check]")
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
	dbs, err := listDBs(cfg)
	if err != nil {
		return rep.report(err)
	}
	if *check {
		fmt.Fprintf(stderr, "config ok: vm %s, %d databases, %d keys (%s)\n", cfg.VM, len(dbs), len(keys.KIDs()),
			strings.Join(slices.Sorted(slices.Values(keys.KIDs())), ", "))
		return exitOK
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "interrupted before serving")
		return exitInterrupted
	}
	a, err := newAgent(cfg, log, keys, dbs)
	if err != nil {
		return rep.report(err)
	}
	defer a.cleanup()
	return a.serve(ctx, *addrFile, rep, stderr)
}

// agent is what serve runs: the pipeline, its host, the lifecycle endpoints and the background jobs.
type agent struct {
	cfg        *Config
	log        *slog.Logger
	srv        *front.Server
	host       *front.StaticHost
	lifecycle  *http.ServeMux              // /livez, /readyz, /healthz; on the lifecycle listener
	background []func(ctx context.Context) // started once listening; stopped first at shutdown
	cleanup    func()
}

// newAgent wires the pipeline. Databases open in the background once serve listens (startup); the canary
// database opens here, so readiness can run from the start.
func newAgent(cfg *Config, log *slog.Logger, keys *ticket.KeyRing, dbs []string) (*agent, error) {
	host := front.NewStaticHost(nil)
	canary, cleanCanary, err := openCanary(cfg)
	if err != nil {
		return nil, err
	}
	host.Add(front.CanaryDB, canary)
	st := &startup{}
	st.pending.Store(int64(len(dbs)))
	srv := front.NewServer(front.Config{
		Limits: cfg.Limits, Admission: cfg.Admission, DefaultTimeout: cfg.DefaultTimeout.Duration,
		MaxTimeout: cfg.MaxTimeout.Duration, StmtTimeout: cfg.StatementTimeout.Duration,
		BodyReadTimeout: cfg.BodyReadTimeout.Duration, WriteGrace: cfg.ReplyGrace.Duration + time.Second,
	}, ticket.NewCache(ticket.Verifier{Keys: keys, VM: cfg.VM, Skew: cfg.ClockSkew.Duration, MaxTTL: cfg.MaxTicketTTL.Duration}, cfg.TicketCache),
		depAdmitter{startupAdmitter{epochs(dbs, cfg), st}}, depExecutor{front.ActorExecutor{Host: host, Grace: cfg.ReplyGrace.Duration}}, nil)
	probe := cfg.ReadyTimeout.Duration
	ready := front.NewReadiness(probe, 3*cfg.ReadyInterval.Duration, nil,
		front.Check{Name: "startup", Run: st.readyCheck},
		front.Check{Name: "canary", Run: func(ctx context.Context) error { return srv.Canary(ctx, front.CanaryDB, probe*2/3) }},
		// CP-1 wiring point: one breakerCheck per remote dependency (S3, placement), and a periodic HEAD of an S3
		// canary object.
	)
	mux := http.NewServeMux()
	mux.Handle(front.HealthPath, front.HealthHandler(srv))
	mux.Handle(front.LivePath, front.LiveHandler(srv))
	mux.Handle(front.ReadyPath, front.ReadyHandler(srv, ready))
	return &agent{cfg: cfg, log: log, srv: srv, host: host, lifecycle: mux, cleanup: cleanCanary,
		background: []func(context.Context){
			func(ctx context.Context) { st.openAll(ctx, cfg, dbs, host, log) },
			func(ctx context.Context) { ready.Run(ctx, cfg.ReadyInterval.Duration, logReadiness(log)) },
		}}, nil
}

// logReadiness logs readiness transitions only (Release It O3: transitions, not every probe).
func logReadiness(log *slog.Logger) func(prev, cur front.ReadyState) {
	return func(prev, cur front.ReadyState) {
		switch {
		case cur.Ready && !prev.Ready:
			log.Info("ready", "event", "AGENT.READY")
		case !cur.Ready && prev.Ready:
			log.Warn("not ready", "event", "AGENT.NOT_READY", "failed", cur.Failed)
		}
	}
}

// serve runs the listeners until ctx ends, then shuts down within cfg.ShutdownBudget: stop the background
// jobs, drain the front (at most drain_timeout), close the host with what is left, then close the lifecycle
// listener. Without a lifecycle listener (health_listen), its endpoints share the query listener.
func (a *agent) serve(ctx context.Context, addrFile string, rep reporter, stderr io.Writer) int {
	cfg, srv, log := a.cfg, a.srv, a.log
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		a.host.Close()
		return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.Listen))
	}
	protos := new(http.Protocols)
	protos.SetHTTP1(slices.Contains(cfg.Protocols, "h1"))
	protos.SetUnencryptedHTTP2(slices.Contains(cfg.Protocols, "h2c"))
	var handler http.Handler = front.NewHandler(srv, log)
	if cfg.HealthListen == "" {
		mux := http.NewServeMux()
		mux.Handle(front.LivePath, a.lifecycle)
		mux.Handle(front.ReadyPath, a.lifecycle)
		mux.Handle("/", handler)
		handler = mux
	}
	hs := &http.Server{Handler: handler, Protocols: protos, ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration,
		IdleTimeout: cfg.IdleTimeout.Duration, MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	servers := []*http.Server{hs}
	listeners := []net.Listener{ln}
	if cfg.HealthListen != "" {
		hl, err := net.Listen("tcp", cfg.HealthListen)
		if err != nil {
			ln.Close()
			a.host.Close()
			return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.HealthListen))
		}
		servers = append(servers, healthServer(cfg, a.lifecycle))
		listeners = append(listeners, hl)
	}
	errc := make(chan error, len(servers))
	for i, s := range servers {
		go func() { errc <- s.Serve(listeners[i]) }()
	}
	addr := ln.Addr().String()
	if addrFile != "" {
		lines := addr + "\n"
		if len(listeners) > 1 {
			lines += listeners[1].Addr().String() + "\n"
		}
		if err := os.WriteFile(addrFile, []byte(lines), 0o644); err != nil {
			for _, s := range servers {
				s.Close()
			}
			a.host.Close()
			return rep.report(errs.Wrap(module, err, "cannot write addr-file").With("path", addrFile))
		}
	}
	log.Info("serving", "addr", addr, "health_addr", cfg.HealthListen, "vm", cfg.VM, "protocols", cfg.Protocols)
	fmt.Fprintf(stderr, "agentd: serving on %s\n", addr)
	bctx, stopBackground := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	for _, job := range a.background {
		bg.Go(func() { job(bctx) })
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	log.Info("shutting down", "event", "AGENT.SHUTDOWN_START", "budget_ms", cfg.ShutdownBudget.Milliseconds(),
		"drain_timeout_ms", cfg.DrainTimeout.Milliseconds(), "in_flight", srv.InFlight())
	err = shutdown(log, cfg.ShutdownBudget.Duration, []phase{
		{"background.stop", 0, func(ctx context.Context) error {
			stopBackground()
			return waitCtx(ctx, &bg, "background jobs (startup, readiness) did not stop in time")
		}},
		{"front.drain", cfg.DrainTimeout.Duration, func(ctx context.Context) error { return drain(ctx, srv, hs, log) }},
		{"host.close", 0, a.host.Shutdown}, // queued work is answered (shed), then every database is closed
		// CP-1 wiring point: the owner table needs no phase of its own (each of its transitions is durable when
		// made), but a vactor host's Shutdown goes here: write back oldest-dirty first, keep the rest locally.
		{"lifecycle.close", 0, func(context.Context) error {
			for _, s := range servers[1:] {
				s.Close()
			}
			return nil
		}},
	})
	switch {
	case serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed):
		return rep.report(errs.Wrap(module, serveErr, "listener failed; stopped"))
	case err != nil:
		return rep.report(err)
	}
	fmt.Fprintln(stderr, "agentd: stopped")
	return exitOK
}

// drain stops the front: new requests are shed (and /healthz says draining), the query listener stops
// accepting, and in-flight requests get until ctx ends to finish.
func drain(ctx context.Context, srv *front.Server, hs *http.Server, log *slog.Logger) error {
	idle := srv.Drain()
	forced := hs.Shutdown(ctx) != nil // stops accepting, waits for in-flight requests (bounded)
	if forced {
		hs.Close()
	}
	select {
	case <-idle:
	case <-ctx.Done():
		forced = true
	}
	left := srv.InFlight()
	log.Info("stopped", "event", "AGENT.DRAIN_END", "forced", forced, "in_flight_left", left)
	if forced {
		return errs.Wrap(module, ctx.Err(), "drain did not finish in time; %d requests were cut off (writes among them may or may not have committed)", left)
	}
	return nil
}

// waitCtx waits for wg until ctx ends.
func waitCtx(ctx context.Context, wg *sync.WaitGroup, msg string) error {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errs.Wrap(module, ctx.Err(), "%s", msg)
	}
}

// healthServer serves the lifecycle endpoints alone. Health requests have no body and a tiny answer, so plain server-wide
// timeouts bound every phase: a stuck or slow peer cannot hold its connection (Release It B4, T1).
func healthServer(cfg *Config, h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration, ReadTimeout: cfg.ReadHeaderTimeout.Duration,
		WriteTimeout: healthWriteTimeout, IdleTimeout: cfg.IdleTimeout.Duration, MaxHeaderBytes: 16 << 10}
}

// healthWriteTimeout bounds one health answer (a few bytes) from the end of its headers.
const healthWriteTimeout = 5 * time.Second

// listDBs returns the database ids to serve: cfg.DBs, or every <id>.db in db_dir.
func listDBs(cfg *Config) ([]string, error) {
	if len(cfg.DBs) > 0 {
		for _, db := range cfg.DBs {
			if _, err := os.Stat(dbPath(cfg, db)); err != nil {
				return nil, errs.Wrap(module, err, "database %s is listed in dbs but missing", db).With("db", db)
			}
		}
		return cfg.DBs, nil
	}
	matches, err := filepath.Glob(filepath.Join(cfg.DBDir, "*.db"))
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot list db_dir").With("dir", cfg.DBDir)
	}
	var out []string
	for _, m := range matches {
		id := strings.TrimSuffix(filepath.Base(m), ".db")
		if err := front.CheckDBID(id); err != nil {
			return nil, errs.Wrap(module, err, "file %s does not name a valid database id", m)
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errs.Wrap(module, nil, "no *.db files in db_dir %s", cfg.DBDir).With("dir", cfg.DBDir)
	}
	return out, nil
}

func dbPath(cfg *Config, db string) string { return filepath.Join(cfg.DBDir, db+".db") }

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

// epochs is the stand-in ownership table until fencing's owner store is wired in: every served database at
// default_epoch unless epochs overrides it.
func epochs(dbs []string, cfg *Config) front.StaticAdmitter {
	t := front.StaticAdmitter{}
	for _, db := range dbs {
		t[db] = cfg.DefaultEpoch
		if e, ok := cfg.Epochs[db]; ok {
			t[db] = e
		}
	}
	return t
}

// reporter classifies an error at the top level and reports it once (the cmd/sqlbench pattern): an outermost
// error from a known module is an expected failure (exit 1), anything else a bug (exit 70). The user gets one
// line with a log ID; the log gets the chain and the stack. Before the log exists a bug prints its details.
type reporter struct {
	log    *slog.Logger
	stderr io.Writer
}

var wellFormed = map[string]bool{module: true, "actor": true, "front": true}

func (r reporter) report(err error) int {
	if e, ok := errs.Outermost(err); ok && wellFormed[e.Module] {
		fmt.Fprintf(r.stderr, "error: %s%s\n", e.Error(), r.record(slog.LevelWarn, "agentd failed", err, errs.Stack(err)))
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
