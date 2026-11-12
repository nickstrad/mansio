// Command agentd is the VM agent (one per VM): one actor per SQLite database behind ticket
// verification, admission and the wire codes of internal/front. Requests use the INTERNAL, TRUSTED-CALLER
// protocol (parameterized SQL); see package front.
//
// Two modes. Durable (an "s3" section): databases live in S3 behind a virtual-actor
// host (internal/vactor), ownership is the owner table (internal/owner) over a single-node placement authority
// (internal/placement/file), and the control plane calls Claim, Fence, Forget and Health on the lifecycle
// listener (control.go). Static (db_dir; benchmarks): the *.db files in one directory at fixed epochs.
//
//	agentd -config configs/agentd/local.json [-addr-file path] [-check] [-dev]
//
// -dev allows what only a developer laptop may do: durable mode over plain-HTTP S3 (s3.secure false; refused
// otherwise: TLS outside a laptop), a plain-HTTP keys_url, and the claim's "create" stand-in (control.go).
//
// Ticket keys come from keys_file (static; dev and benches) or keys_url (controld's GET /v1/keys, refetched
// every keys_refresh and cached in keys_file; keys.go). An optional admin
// listener (admin_listen, loopback only) serves the runtime controls of admin.go; cmd/agentctl is its client.
//
// Exit codes: 0 stopped cleanly after SIGINT/SIGTERM (drained), 1 failed (message and log ID on stderr; this
// includes a shutdown that did not finish cleanly: a drain cut off at drain_timeout, or databases not closed within
// shutdown_budget), 2 bad usage, 70 bug (report the log ID),
// 130 interrupted before it started serving.
package main

import (
	"context"
	"crypto/tls"
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

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/agentglue"
	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/eventlog"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/panicx"
	"github.com/nickstrad/mansio/server/internal/slots"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/vactor"
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
	addrFile := fs.String("addr-file", "", "write the query listener's address here once serving, the lifecycle listener's on a second line "+
		"(the query listener's when they share it) and the admin listener's on a third (if any)")
	check := fs.Bool("check", false, "validate the config, keys and databases, then exit")
	dev := fs.Bool("dev", false, "local development: allow plain-HTTP S3 (s3.secure false) and claim's create stand-in")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || *cfgPath == "" {
		fmt.Fprintln(stderr, "usage: agentd -config <file> [-addr-file <file>] [-check] [-dev]")
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
	level := new(slog.LevelVar)
	log, closeLog, err := openLog(cfg, level, stderr)
	if err != nil {
		return rep.report(err)
	}
	defer closeLog()
	rep.log = log
	keys, fetcher, err := openKeys(ctx, cfg, log)
	if err != nil {
		return rep.report(err)
	}
	queryTLS, err := cfg.queryTLS()
	if err != nil {
		return rep.report(err)
	}
	var dbs []string
	var sec *Secrets
	if cfg.Durable() {
		sec, err = loadSecrets(cfg.SecretsFile)
	} else {
		dbs, err = listDBs(cfg)
	}
	if err != nil {
		return rep.report(err)
	}
	if *check {
		what := fmt.Sprintf("%d databases", len(dbs))
		if cfg.Durable() {
			what = fmt.Sprintf("durable mode, s3 %s bucket %s", cfg.S3.Endpoint, cfg.S3.Bucket)
		}
		fmt.Fprintf(stderr, "config ok: vm %s, %s, %d keys (%s)\n", cfg.VM, what, len(keys.KIDs()),
			strings.Join(slices.Sorted(slices.Values(keys.KIDs())), ", "))
		return exitOK
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "interrupted before serving")
		return exitInterrupted
	}
	a, err := newAgent(cfg, log, keys, dbs, sec)
	if err != nil {
		return rep.report(err)
	}
	if fetcher != nil {
		fetcher.Register(a.reg, "agentd")
		a.background = append(a.background, fetcher.Run)
	}
	a.level = level
	a.tls = queryTLS
	defer a.cleanup()
	return a.serve(ctx, *addrFile, rep, stderr)
}

// agent is what serve runs: the pipeline, its backend (static or durable mode), the lifecycle endpoints and
// the background jobs.
type agent struct {
	cfg        *Config
	log        *slog.Logger
	srv        *front.Server
	be         *backend
	lifecycle  *http.ServeMux              // /livez, /readyz, /healthz, /metrics, control endpoints; on the lifecycle listener
	reg        *metrics.Registry           // served on /metrics
	level      *slog.LevelVar              // the log level; POST /admin/loglevel sets it
	tls        *tls.Config                 // the query listener's TLS (tls.go); nil = plain HTTP
	background []func(ctx context.Context) // started once listening; stopped first at shutdown
	cleanup    func()
	events     *eventlog.Log // the event log (events.go); nil = none
	// queryRoutes adds routes beside the front's on the query listener (tenancy's /admin/ and /api/auth/); may be nil.
	queryRoutes func(mux *http.ServeMux) error
	tenancy     *tenancyPart // its slot pools exist once queryRoutes mounted them
}

// backend is what a mode plugs into the pipeline (static: local files, durable: S3 + owner table).
type backend struct {
	schemas    front.SchemaSource // creates new databases on /v1/ops (registry or schemas_dir); nil = none
	admitter   front.Admitter
	executor   front.Executor
	ready      func(ctx context.Context) error // the "startup" readiness check
	pending    func() int                      // databases not served yet since start (metrics)
	metrics    func(reg *metrics.Registry)
	routes     func(mux *http.ServeMux, srv *front.Server) // extra lifecycle-listener endpoints; may be nil
	background []func(ctx context.Context)
	close      func(ctx context.Context) error // the host.close shutdown phase
	abort      func()                          // closes everything if serving never started
	// The admin endpoints' handles (admin.go): the remote-dependency breakers by name, and the host (nil in
	// static mode: no breaker, fetch-limit or passivate controls).
	breakers map[string]*breaker.Breaker
	host     *vactor.Host
	// limits are the mode's slot pools an operator may resize (POST /admin/limit), by queue name; nil in static mode.
	limits map[string]*slots.Pool
	// backup forces a key's write-back and answers its manifest (POST /admin/backup, backup.go); nil in static mode.
	backup func(ctx context.Context, key string) (backupAnswer, error)
	// credentialPlace is durable mode's front.OpsConfig.CredentialPlace (tenancy.go); nil in static mode.
	credentialPlace func(ctx context.Context, db string) (epoch, gen uint64, err error)
}

// newAgent wires the pipeline. Databases open (static) or reconcile (durable) in the background once serve
// listens; the canary database opens here, so readiness can run from the start.
func newAgent(cfg *Config, log *slog.Logger, keys *ticket.KeyRing, dbs []string, sec *Secrets) (*agent, error) {
	events, err := openEventLog(cfg, log)
	if err != nil {
		return nil, err
	}
	cfg.actorQueues = actor.NewQueues()
	canary, cleanCanary, err := openCanary(cfg)
	if err != nil {
		closeEventLog(events, time.Second)
		return nil, err
	}
	canaryHost := front.NewStaticHost(map[string]*actor.Actor{front.CanaryDB: canary})
	var be *backend
	if cfg.Durable() {
		be, err = newDurable(cfg, log, sec, canaryHost)
	} else {
		be = newStatic(cfg, log, dbs, canaryHost)
		be.schemas, err = loadSchemaDir(cfg.SchemasDir)
	}
	if err != nil {
		canaryHost.Close()
		cleanCanary()
		closeEventLog(events, time.Second)
		return nil, err
	}
	srv := front.NewServer(front.Config{
		Limits: cfg.Limits, Admission: cfg.Admission, DefaultTimeout: cfg.DefaultTimeout.Duration,
		MaxTimeout: cfg.MaxTimeout.Duration, StmtTimeout: cfg.StatementTimeout.Duration,
		BodyReadTimeout: cfg.BodyReadTimeout.Duration, WriteGrace: cfg.ReplyGrace.Duration + time.Second,
	}, ticket.NewCache(ticket.Verifier{Keys: keys, VM: cfg.VM, Skew: cfg.ClockSkew.Duration, MaxTTL: cfg.MaxTicketTTL.Duration}, cfg.TicketCache),
		agentglue.DepAdmitter{Admitter: be.admitter}, agentglue.DepExecutor{Executor: be.executor}, nil)
	ten := &tenancyPart{ready: func(context.Context) error { return nil }, close: func() {}}
	if cfg.Tenancy != nil {
		if ten, err = newTenancy(cfg, log, be.schemas); err != nil {
			canaryHost.Close() // static mode: the backend's host
			cleanCanary()
			closeEventLog(events, time.Second)
			return nil, err
		}
		if ten.breaker != nil {
			if be.breakers == nil {
				be.breakers = map[string]*breaker.Breaker{}
			}
			be.breakers["tenancy"] = ten.breaker
		}
	}
	ops := front.OpsConfig{Schemas: be.schemas, SchemaCache: cfg.SchemaCache, Credentials: ten.creds, CredentialEpoch: cfg.DefaultEpoch,
		CredentialPlace: be.credentialPlace, VerifySlots: cfg.VerifySlots}
	if events != nil {
		ops.Observer = opsObserver(events, cfg.EventLog.TraceSQL)
	}
	srv.EnableOps(ops)
	probe := cfg.ReadyTimeout.Duration
	ready := front.NewReadiness(probe, 3*cfg.ReadyInterval.Duration, nil,
		front.Check{Name: "startup", Run: be.ready},
		front.Check{Name: "tenancy", Run: ten.ready},
		front.Check{Name: "canary", Run: func(ctx context.Context) error { return srv.Canary(ctx, front.CanaryDB, probe*2/3) }},
		// Remote dependency breakers (S3, placement) never fail readiness: warm traffic still works while they
		// are open, so an outage must not pull the agent from the load balancer. They are on /metrics instead.
	)
	reg := metrics.NewRegistry()
	metrics.Process(reg)
	frontMetrics(reg, srv, cfg)
	lifecycleMetrics(reg, be.pending, ready, time.Now())
	be.metrics(reg)
	if cfg.Tenancy != nil {
		metrics.Queues(reg, ten.pools.Queues)
	}
	eventLogMetrics(reg, events)
	mux := http.NewServeMux()
	mux.Handle(metricsPath, reg.Handler())
	mux.Handle(front.HealthPath, front.HealthHandler(srv))
	mux.Handle(front.LivePath, front.LiveHandler(srv))
	mux.Handle(front.ReadyPath, front.ReadyHandler(srv, ready))
	if be.routes != nil {
		be.routes(mux, srv)
	}
	// The event log closes last, after the shutdown phases, with its own fixed 2 s on top of shutdown_budget:
	// it only writes what is already queued (a buffered file; no remote dependency), and records still queued
	// at the end are lost, counted, never waited for.
	cleanup := func() {
		ten.close()
		if err := closeEventLog(events, 2*time.Second); err != nil {
			log.Warn("event log not closed cleanly", "event", "EVENTLOG.CLOSE_FAILED", "error", err.Error())
		}
		cleanCanary()
	}
	return &agent{cfg: cfg, log: log, srv: srv, be: be, lifecycle: mux, reg: reg, cleanup: cleanup, events: events, queryRoutes: ten.mount, tenancy: ten,
		background: append(append(be.background, ten.background...),
			func(ctx context.Context) { ready.Run(ctx, cfg.ReadyInterval.Duration, logReadiness(log)) })}, nil
}

// newStatic is static mode: the databases in db_dir, opened in the background (startup), owned at fixed epochs.
func newStatic(cfg *Config, log *slog.Logger, dbs []string, host *front.StaticHost) *backend {
	host.Expect(dbs...)
	st := &startup{host: host}
	table := epochs(dbs, cfg)
	var admitter front.Admitter = startupAdmitter{table, host}
	var lookup front.Host = host
	if cfg.CreateDBs {
		admitter = creatingAdmitter{Admitter: admitter, table: table, epoch: cfg.DefaultEpoch}
		lookup = newCreatingHost(host, cfg, dbs)
	}
	return &backend{
		admitter: admitter,
		executor: front.ActorExecutor{Host: lookup, Grace: cfg.ReplyGrace.Duration},
		ready:    st.readyCheck,
		pending:  host.PendingCount,
		metrics:  func(reg *metrics.Registry) { actorMetrics(reg, host.QueueDepths) },
		background: []func(context.Context){
			func(ctx context.Context) { st.openAll(ctx, cfg, dbs, log) },
		},
		close: host.Shutdown, // queued work is answered (shed), then every database is closed
		abort: host.Close,
	}
}

// logReadiness logs readiness transitions only, not every probe, so the log stays readable for operators.
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
	served := false
	defer func() {
		if !served { // failed before serving: the shutdown phases never ran
			a.be.abort()
		}
	}()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.Listen))
	}
	protos := new(http.Protocols)
	protos.SetHTTP1(slices.Contains(cfg.Protocols, "h1") || a.tls != nil)
	protos.SetUnencryptedHTTP2(slices.Contains(cfg.Protocols, "h2c") && a.tls == nil)
	protos.SetHTTP2(a.tls != nil)
	handler := front.NewRoutes(srv, log)
	if cfg.HealthListen == "" || a.queryRoutes != nil {
		mux := http.NewServeMux()
		if cfg.HealthListen == "" {
			mux.Handle(front.LivePath, a.lifecycle)
			mux.Handle(front.ReadyPath, a.lifecycle)
			mux.Handle(metricsPath, a.lifecycle)
		}
		if a.queryRoutes != nil {
			if err := a.queryRoutes(mux); err != nil {
				ln.Close()
				return rep.report(err)
			}
		}
		mux.Handle("/", handler)
		handler = mux
	}
	// One deadline wrapper around every route on this listener, the tenancy and lifecycle ones included: the
	// server below has no read or write timeout of its own.
	handler = srv.Deadlines(handler)
	hs := &http.Server{Handler: handler, Protocols: protos, ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration,
		IdleTimeout: cfg.IdleTimeout.Duration, MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	servers := []*http.Server{hs}
	listeners := []net.Listener{tlsListener(ln, a.tls)}
	closeAll := func() {
		for _, l := range listeners {
			l.Close()
		}
	}
	lifeAddr := ln.Addr().String()
	if cfg.HealthListen != "" {
		hl, err := net.Listen("tcp", cfg.HealthListen)
		if err != nil {
			closeAll()
			return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.HealthListen))
		}
		servers = append(servers, healthServer(cfg, a.lifecycle))
		listeners = append(listeners, hl)
		lifeAddr = hl.Addr().String()
	}
	addrLines := ln.Addr().String() + "\n" + lifeAddr + "\n"
	if cfg.AdminListen != "" {
		al, err := net.Listen("tcp", cfg.AdminListen)
		if err != nil {
			closeAll()
			return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.AdminListen))
		}
		servers = append(servers, healthServer(cfg, a.adminHandler()))
		listeners = append(listeners, al)
		addrLines += al.Addr().String() + "\n"
	}
	errc := make(chan error, len(servers))
	for i, s := range servers {
		go func() { errc <- s.Serve(listeners[i]) }()
	}
	addr := ln.Addr().String()
	if addrFile != "" {
		if err := os.WriteFile(addrFile, []byte(addrLines), 0o644); err != nil {
			for _, s := range servers {
				s.Close()
			}
			return rep.report(errs.Wrap(module, err, "cannot write addr-file").With("path", addrFile))
		}
	}
	log.Info("serving", "addr", addr, "health_addr", cfg.HealthListen, "admin_addr", cfg.AdminListen, "vm", cfg.VM, "protocols", cfg.Protocols,
		"dev", cfg.Dev)
	fmt.Fprintf(stderr, "agentd: serving on %s\n", addr)
	a.events.Emit(eventlog.Event{TraceID: lifecycleTrace, Payload: eventlog.Lifecycle{Phase: "startup",
		Detail: map[string]any{"addr": addr, "vm": cfg.VM, "durable": cfg.Durable()}}})
	bctx, stopBackground := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	for _, job := range a.background {
		bg.Go(func() { job(bctx) })
	}

	served = true
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
		// Static: queued work is answered (shed), then every database is closed. Durable: the vactor host writes
		// back oldest-dirty first within the shutdown budget and keeps the rest locally; the owner table needs no
		// phase (each of its transitions is durable when made).
		{"host.close", 0, a.be.close},
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
// timeouts bound every phase: a stuck or slow peer cannot hold its connection, and health stays answerable.
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
	if len(out) == 0 && !cfg.CreateDBs {
		return nil, errs.Wrap(module, nil, "no *.db files in db_dir %s", cfg.DBDir).With("dir", cfg.DBDir)
	}
	return out, nil
}

func dbPath(cfg *Config, db string) string { return filepath.Join(cfg.DBDir, db+".db") }

// logLevels are the log_level values (and /admin/loglevel's).
var logLevels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn}

// openLog opens the log at cfg.LogLevel, held in level so it can change at run time.
func openLog(cfg *Config, level *slog.LevelVar, stderr io.Writer) (*slog.Logger, func(), error) {
	level.Set(logLevels[cfg.LogLevel])
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

// epochs is static mode's ownership table: every served database at
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

// reporter classifies an error at the top level and reports it once (the cmd/bench/sqlbench pattern): an outermost
// error from a known module is an expected failure (exit 1), anything else a bug (exit 70). The user gets one
// line with a log ID; the log gets the chain and the stack. Before the log exists a bug prints its details.
type reporter struct {
	log    *slog.Logger
	stderr io.Writer
}

var wellFormed = map[string]bool{module: true, "actor": true, "front": true, "vactor": true, "owner": true, "objstore": true,
	"placement": true, "durable": true, "tenancy": true}

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
