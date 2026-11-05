package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/control"
	"github.com/nickstrad/ts-db-go/internal/control/agenthttp"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/panicx"
	"github.com/nickstrad/ts-db-go/internal/parallel"
	"github.com/nickstrad/ts-db-go/internal/placement/pg"
)

// daemon is a running controld: the authority, the controller and one client per agent.
type daemon struct {
	cfg    *Config
	log    *slog.Logger
	pg     *pg.Placement
	ctl    *control.Controller
	agents map[string]*agenthttp.Client
	token  string // the API's admin token
	sem    chan struct{}

	ready      atomic.Bool // schema migrated and agents registered
	agentUp    sync.Map    // vm → bool: last health answer ok (transitions logged)
	resumed    atomic.Int64
	resumeFail atomic.Int64 // keys that failed in the last pass
}

func serveMain(ctx context.Context, args []string, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("controld", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "controld config (JSON)")
	addrFile := fs.String("addr-file", "", "write the API listener's address here once serving")
	dev := fs.Bool("dev", false, "local development: allow plain-HTTP S3 (s3.secure false)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || *cfgPath == "" {
		fmt.Fprintln(stderr, "usage: controld -config <file> [-addr-file <file>] [-dev] | controld <command> -addr URL -secrets FILE ...")
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
	sec, err := loadSecrets(cfg.SecretsFile)
	if err != nil {
		return rep.report(err)
	}
	if ctx.Err() != nil {
		return exitInterrupted
	}
	d, err := newDaemon(cfg, log, sec)
	if err != nil {
		return rep.report(err)
	}
	defer d.pg.Close()
	return d.serve(ctx, *addrFile, rep, stderr)
}

func newDaemon(cfg *Config, log *slog.Logger, sec *Secrets) (*daemon, error) {
	p, err := pg.Open(pg.Config{DSN: sec.PGDSN, AppName: "controld", Schema: cfg.PG.Schema, MaxConns: cfg.PG.MaxConns,
		Breaker: breaker.Config{OnChange: logx.BreakerLog(log, "placement")}})
	if err != nil {
		return nil, err
	}
	st, err := objstore.New(objstore.Config{Endpoint: cfg.S3.Endpoint, Bucket: cfg.S3.Bucket, AccessKey: sec.S3AccessKey,
		SecretKey: sec.S3SecretKey, OpTimeout: cfg.S3.OpTimeout.Duration, Secure: cfg.S3.Secure, CAFile: cfg.S3.CAFile, Logger: log})
	if err != nil {
		p.Close()
		return nil, err
	}
	d := &daemon{cfg: cfg, log: log, pg: p, agents: map[string]*agenthttp.Client{}, token: sec.AdminToken,
		sem: make(chan struct{}, cfg.APIConcurrency)}
	httpc := agenthttp.DefaultHTTP()
	for _, a := range cfg.Agents {
		c, err := agenthttp.New(agenthttp.Config{VM: a.VM, BaseURL: a.Lifecycle, Token: sec.ControlToken, Timeout: cfg.AgentTimeout.Duration,
			HTTP: httpc, Breaker: breaker.Config{MinCalls: cfg.AgentBreaker.MinCalls, CoolDown: cfg.AgentBreaker.CoolDown.Duration,
				OnChange: logx.BreakerLog(log, "agent:"+a.VM)}})
		if err != nil {
			p.Close()
			return nil, err
		}
		d.agents[a.VM] = c
	}
	pol := cfg.Policy
	d.ctl = &control.Controller{Registry: registry{p}, Placement: p, Objects: objects{st, cfg.S3.Prefix},
		Policy: control.Policy{MaxDatabases: pol.MaxDatabases, MaxBytes: pol.MaxBytes, MaxHeartbeatAge: pol.MaxHeartbeatAge.Duration},
		Agents: func(vm string) (control.Agent, error) {
			if c, ok := d.agents[vm]; ok {
				return c, nil
			}
			return nil, errs.Wrap(module, nil, "vm %q is not in controld's agents list", vm)
		}}
	return d, nil
}

// serve runs the API until ctx ends, with the startup job (migrate, register agents), the heartbeat poller
// and the resumer in the background (Release It H1: serve first, become ready when startup is done).
func (d *daemon) serve(ctx context.Context, addrFile string, rep reporter, stderr io.Writer) int {
	ln, err := net.Listen("tcp", d.cfg.Listen)
	if err != nil {
		return rep.report(errs.Wrap(module, err, "cannot listen on %s", d.cfg.Listen))
	}
	reg := metrics.NewRegistry()
	d.register(reg)
	hs := &http.Server{Handler: d.routes(reg), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: d.cfg.APITimeout.Duration + 5*time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(d.log.Handler(), slog.LevelWarn)}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	if addrFile != "" {
		if err := os.WriteFile(addrFile, []byte(ln.Addr().String()+"\n"), 0o644); err != nil {
			hs.Close()
			return rep.report(errs.Wrap(module, err, "cannot write addr-file").With("path", addrFile))
		}
	}
	d.log.Info("serving", "event", "CONTROLD.SERVING", "addr", ln.Addr().String(), "agents", len(d.agents))
	fmt.Fprintf(stderr, "controld: serving on %s\n", ln.Addr())
	bctx, stop := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	bg.Go(func() { d.startup(bctx) })
	bg.Go(func() { d.every(bctx, d.cfg.HeartbeatInterval.Duration, d.heartbeat) })
	bg.Go(func() { d.every(bctx, d.cfg.ResumeInterval.Duration, d.resume) })
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), d.cfg.ShutdownBudget.Duration)
	defer cancel()
	stop()
	shutErr := hs.Shutdown(sctx)
	done := make(chan struct{})
	go func() { bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-sctx.Done():
		shutErr = errors.Join(shutErr, errors.New("background jobs did not stop in time"))
	}
	switch {
	case serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed):
		return rep.report(errs.Wrap(module, serveErr, "listener failed; stopped"))
	case shutErr != nil:
		return rep.report(errs.Wrap(module, shutErr, "shutdown did not finish within %v", d.cfg.ShutdownBudget))
	}
	fmt.Fprintln(stderr, "controld: stopped")
	return exitOK
}

// startup migrates the schema and registers the configured agents, retrying with jittered backoff until it
// succeeds or ctx ends. The API answers unavailable until then.
func (d *daemon) startup(ctx context.Context) {
	backoff := 100 * time.Millisecond
	for {
		err := d.startOnce(ctx)
		if err == nil {
			d.ready.Store(true)
			d.log.Info("ready", "event", "CONTROLD.READY")
			return
		}
		d.log.Warn("startup failed; retrying", "event", "CONTROLD.STARTUP_RETRY", "error", err.Error(), "backoff_ms", backoff.Milliseconds())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff/2 + rand.N(backoff/2)):
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

func (d *daemon) startOnce(ctx context.Context) error {
	v, err := d.pg.Migrate(ctx)
	if err != nil {
		return err
	}
	for _, a := range d.cfg.Agents {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := d.pg.RegisterVM(rctx, a.VM, a.Endpoint)
		cancel()
		if err != nil {
			return err
		}
	}
	d.log.Info("schema migrated and agents registered", "event", "CONTROLD.REGISTERED", "schema_version", v, "agents", len(d.cfg.Agents))
	return nil
}

// every runs fn now and then every period until ctx ends; a run never overlaps the next (Release It S1).
func (d *daemon) every(ctx context.Context, period time.Duration, fn func(context.Context)) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		if d.ready.Load() {
			fn(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// heartbeat polls every agent's health in parallel, each bounded by heartbeat_timeout, and records the
// answers as heartbeats (the VM registry's liveness and bytes, the policy's input). A silent agent keeps its
// old heartbeat, so the policy stops placing on it after max_heartbeat_age.
func (d *daemon) heartbeat(ctx context.Context) {
	vms := slices.Sorted(maps.Keys(d.agents))
	parallel.ForEach(ctx, len(vms), len(vms), func(ctx context.Context, i int) error {
		vm := vms[i]
		hctx, cancel := context.WithTimeout(ctx, d.cfg.HeartbeatTimeout.Duration)
		defer cancel()
		h, err := d.agents[vm].Health(hctx)
		if err == nil {
			err = d.pg.Heartbeat(hctx, vm, h.Bytes)
		}
		if ctx.Err() != nil {
			return nil
		}
		up := err == nil
		if prev, ok := d.agentUp.Swap(vm, up); !ok || prev.(bool) != up {
			if up {
				d.log.Info("agent answers", "event", "CONTROLD.AGENT_UP", "vm", vm)
			} else {
				d.log.Warn("agent heartbeat failed", "event", "CONTROLD.AGENT_DOWN", "vm", vm, "error", err.Error())
			}
		}
		return nil
	})
}

// resume is one resumer pass (control.Controller.ResumeStuck). Each failure is logged at WARN; the next
// pass retries it.
func (d *daemon) resume(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, d.cfg.ResumeInterval.Duration+d.cfg.PerKeyTimeout.Duration)
	defer cancel()
	rep, err := d.ctl.ResumeStuck(rctx, d.cfg.ResumeAfter.Duration, d.cfg.ResumeLimit, d.cfg.PerKeyTimeout.Duration)
	if err != nil && ctx.Err() == nil {
		d.log.Warn("resumer pass failed", "event", "CONTROLD.RESUME_FAILED", "error", err.Error())
	}
	d.resumed.Add(int64(rep.Resumed))
	d.resumeFail.Store(int64(len(rep.Failed)))
	for _, k := range slices.Sorted(maps.Keys(rep.Failed)) {
		d.log.Warn("flow not resumed; next pass retries", "event", "CONTROLD.RESUME_KEY_FAILED", "db", k, "error", rep.Failed[k].Error())
	}
	if rep.Resumed > 0 {
		d.log.Info("flows resumed", "event", "CONTROLD.RESUMED", "count", rep.Resumed)
	}
}

// register adds controld's metrics: breakers (placement, one per agent), agent liveness, resumer counters.
func (d *daemon) register(reg *metrics.Registry) {
	deps := map[string]*breaker.Breaker{"placement": d.pg.Breaker()}
	for vm, c := range d.agents {
		deps["agent:"+vm] = c.Breaker()
	}
	metrics.Breakers(reg, deps)
	reg.Add("controld_agent_up", "1 when the agent's last health call answered.", metrics.Gauge, func(e *metrics.Emit) {
		for _, vm := range slices.Sorted(maps.Keys(d.agents)) {
			v := 0.0
			if up, ok := d.agentUp.Load(vm); ok && up.(bool) {
				v = 1
			}
			e.Value(metrics.Labels{"vm", vm}, v)
		}
	})
	reg.Add("controld_resumed_total", "Flows the resumer finished.", metrics.Counter, func(e *metrics.Emit) {
		e.Value(nil, float64(d.resumed.Load()))
	})
	reg.Add("controld_resume_failed", "Keys the last resumer pass could not finish.", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, float64(d.resumeFail.Load()))
	})
	reg.Add("controld_ready", "1 once the schema is migrated and the agents registered.", metrics.Gauge, func(e *metrics.Emit) {
		v := 0.0
		if d.ready.Load() {
			v = 1
		}
		e.Value(nil, v)
	})
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

// reporter classifies a top-level error once: a wrapped error from a known module is expected (exit 1),
// anything else a bug (exit 70). One line with a log ID for the user; chain and stack in the log.
type reporter struct {
	log    *slog.Logger
	stderr io.Writer
}

var wellFormed = map[string]bool{module: true, "placement": true, "objstore": true, "agenthttp": true, "control": true}

func (r reporter) report(err error) int {
	if e, ok := errs.Outermost(err); ok && wellFormed[e.Module] {
		fmt.Fprintf(r.stderr, "error: %s%s\n", e.Error(), r.record(slog.LevelWarn, "controld failed", err, errs.Stack(err)))
		return exitError
	}
	return r.bug(err, errs.Stack(err))
}

func (r reporter) bug(err error, stack string) int {
	if r.log == nil {
		fmt.Fprintf(r.stderr, "%v\n%s\nerror: something unexpected went wrong. This is a bug; please report the details above\n", err, stack)
		return exitBug
	}
	fmt.Fprintf(r.stderr, "error: something unexpected went wrong. This is a bug; please report%s\n", r.record(slog.LevelError, "unexpected error (bug)", err, stack))
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
