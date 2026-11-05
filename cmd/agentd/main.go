// Command agentd is the VM agent's client-facing front (scale-out option 4 §2): it serves the SQLite databases
// in one directory, one actor per database, behind ticket verification, admission and the wire codes of
// internal/front. Requests use the INTERNAL, TRUSTED-CALLER protocol (parameterized SQL); see package front.
//
//	agentd -config configs/agentd/local.json [-addr-file path] [-check]
//
// Exit codes: 0 stopped cleanly after SIGINT/SIGTERM (drained), 1 failed (message and log ID on stderr; this
// includes a drain that did not finish within drain_timeout), 2 bad usage, 70 bug (report the log ID),
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
	"syscall"

	"github.com/nickstrad/ts-db-go/internal/actor"
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
	addrFile := fs.String("addr-file", "", "write the query listener's address here once serving")
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
	host, err := openHost(ctx, cfg, dbs)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "interrupted while opening databases")
			return exitInterrupted
		}
		return rep.report(err)
	}
	defer host.Close() // after the drain: queued work is answered (shed) and every database is closed

	srv := front.NewServer(front.Config{
		Limits: cfg.Limits, Admission: cfg.Admission, DefaultTimeout: cfg.DefaultTimeout.Duration,
		MaxTimeout: cfg.MaxTimeout.Duration, StmtTimeout: cfg.StatementTimeout.Duration,
	}, ticket.NewCache(ticket.Verifier{Keys: keys, VM: cfg.VM, Skew: cfg.ClockSkew.Duration, MaxTTL: cfg.MaxTicketTTL.Duration}, cfg.TicketCache),
		epochs(dbs, cfg), front.ActorExecutor{Host: host, Grace: cfg.ReplyGrace.Duration}, nil)
	return serve(ctx, cfg, srv, log, *addrFile, rep, stderr)
}

// serve runs the listeners until ctx ends, then drains.
func serve(ctx context.Context, cfg *Config, srv *front.Server, log *slog.Logger, addrFile string, rep reporter, stderr io.Writer) int {
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.Listen))
	}
	protos := new(http.Protocols)
	protos.SetHTTP1(slices.Contains(cfg.Protocols, "h1"))
	protos.SetUnencryptedHTTP2(slices.Contains(cfg.Protocols, "h2c"))
	hs := &http.Server{Handler: front.NewHandler(srv, log), Protocols: protos, ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration,
		IdleTimeout: cfg.IdleTimeout.Duration, MaxHeaderBytes: 16 << 10, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	servers := []*http.Server{hs}
	listeners := []net.Listener{ln}
	if cfg.HealthListen != "" {
		hl, err := net.Listen("tcp", cfg.HealthListen)
		if err != nil {
			ln.Close()
			return rep.report(errs.Wrap(module, err, "cannot listen on %s", cfg.HealthListen))
		}
		mux := http.NewServeMux()
		mux.Handle(front.HealthPath, front.HealthHandler(srv))
		servers = append(servers, &http.Server{Handler: mux, ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration})
		listeners = append(listeners, hl)
	}
	errc := make(chan error, len(servers))
	for i, s := range servers {
		go func() { errc <- s.Serve(listeners[i]) }()
	}
	addr := ln.Addr().String()
	if addrFile != "" {
		if err := os.WriteFile(addrFile, []byte(addr+"\n"), 0o644); err != nil {
			for _, s := range servers {
				s.Close()
			}
			return rep.report(errs.Wrap(module, err, "cannot write addr-file").With("path", addrFile))
		}
	}
	log.Info("serving", "addr", addr, "health_addr", cfg.HealthListen, "vm", cfg.VM, "protocols", cfg.Protocols)
	fmt.Fprintf(stderr, "agentd: serving on %s\n", addr)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	idle := srv.Drain() // new requests are shed; /healthz says draining
	dctx, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout.Duration)
	defer cancel()
	forced := hs.Shutdown(dctx) != nil // stops accepting, waits for in-flight requests (bounded)
	if forced {
		hs.Close()
	}
	for _, s := range servers[1:] {
		s.Close()
	}
	select {
	case <-idle:
	case <-dctx.Done():
		forced = true
	}
	left := srv.InFlight()
	log.Info("stopped", "forced", forced, "in_flight_left", left)
	switch {
	case serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed):
		return rep.report(errs.Wrap(module, serveErr, "listener failed; stopped"))
	case forced:
		return rep.report(errs.Wrap(module, nil, "drain did not finish within %v; %d requests were cut off (writes among them may or may not have committed)",
			cfg.DrainTimeout, left))
	}
	fmt.Fprintln(stderr, "agentd: stopped")
	return exitOK
}

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

// openHost starts one actor per database. All or nothing: on any failure (or ctx ending) every actor opened so
// far is closed.
func openHost(ctx context.Context, cfg *Config, dbs []string) (*front.StaticHost, error) {
	acfg := cfg.Actor.Actor()
	actors := make(map[string]*actor.Actor, len(dbs))
	fail := func(err error) (*front.StaticHost, error) {
		front.NewStaticHost(actors).Close()
		return nil, err
	}
	for i, db := range dbs {
		if ctx.Err() != nil {
			return fail(errs.Wrap(module, ctx.Err(), "interrupted while opening databases"))
		}
		a := actor.New(i, dbPath(cfg, db), acfg)
		if err := a.Start(true); err != nil {
			return fail(errs.Wrap(module, err, "cannot open database %s; nothing is served", db).With("db", db))
		}
		actors[db] = a
	}
	return front.NewStaticHost(actors), nil
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
