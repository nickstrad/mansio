package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/metrics"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/placement/file"
	"github.com/nickstrad/ts-db-go/internal/placement/pg"
	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/actordb"
)

// durableAgent is durable mode (control-plane plan CP-1): one long-lived S3 client behind a breaker, the
// virtual-actor host in durable mode, the owner table over a local FileStore and the single-node placement
// authority, and the glue between them (glue.go).
type durableAgent struct {
	cfg    *Config
	log    *slog.Logger
	s3     *objstore.Store
	s3b    *breaker.Breaker
	raw    durable.Store // the S3 adapter without the breaker (the probe only)
	store  durable.Store // behind the breaker: every other caller
	host   *vactor.Host
	tab    *owner.Table
	place  owner.Placement
	pg     *pg.Placement // the Postgres authority (placement_pg); nil with placement_dir
	canary *front.StaticHost
	ticks  *tickLog
	events *ownerEvents
	probe  s3Probe

	firstRound atomic.Bool // the first reconcile round finished (some keys may still be retrying)
}

// s3Transient is the S3 breaker classifier: only Transient errors (timeouts, connection errors, 5xx, 429)
// are system failures (Release It C2).
func s3Transient(err error) bool { return objstore.Classify(err) == objstore.Transient }

// openTimeout bounds startup's local work, and the bucket check.
const openTimeout = 10 * time.Second

// newDurable wires durable mode. S3 being down does not stop startup: keys stay unreconciled (unavailable)
// until it answers (Release It H1).
func newDurable(cfg *Config, log *slog.Logger, sec *Secrets, canary *front.StaticHost) (*backend, error) {
	s := cfg.S3
	st, err := objstore.New(objstore.Config{Endpoint: s.Endpoint, Bucket: s.Bucket, AccessKey: sec.S3AccessKey, SecretKey: sec.S3SecretKey,
		MaxConns: s.MaxConns, OpTimeout: s.OpTimeout.Duration, StallTimeout: s.StallTimeout.Duration, Logger: log,
		Secure: s.Secure, CAFile: s.CAFile})
	if err != nil {
		return nil, err
	}
	bc := s.Breaker.config() // zero fields: Release It §P S3 sheet
	bc.OnChange = logx.BreakerLog(log, "s3")
	b, err := breaker.New(bc)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot create the S3 breaker")
	}
	raw := objstore.NewDurable(st, s.Prefix)
	d := &durableAgent{cfg: cfg, log: log, s3: st, s3b: b, raw: raw, store: durable.WithBreaker(raw, b, s3Transient, nil),
		canary: canary, ticks: &tickLog{log: log}, events: &ownerEvents{log: log}}
	m := cfg.Maintenance
	d.host, err = vactor.NewHost(nil, actordb.Opener{Cfg: cfg.Actor.Actor()}, vactor.Options{
		CacheDir: cfg.CacheDir, Store: d.store, GenCacheBytes: cfg.GenCacheBytes,
		Policy: vactor.Policy{WriteBackEvery: m.WriteBackEvery.Duration, WriteBackCommits: m.WriteBackCommits,
			IdleAfter: m.IdleAfter.Duration, MaxActive: m.MaxActive, MaxBytes: m.MaxBytes},
		WriteBackTimeout: m.WriteBackTimeout.Duration,
		CheckActivation:  activationCheck(func() *owner.Table { return d.tab }),
		Guard:            publishGuard(func() *owner.Table { return d.tab }, func() *vactor.Host { return d.host }),
	})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*backend, error) {
		d.host.Close()
		d.closePlacement()
		return nil, err
	}
	fs, err := owner.NewFileStore(cfg.OwnerDir)
	if err != nil {
		return fail(err)
	}
	if d.place, err = d.openPlacement(sec); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	d.tab, err = owner.Open(ctx, owner.Config{Self: cfg.VM, Store: fs, Placement: d.place,
		WriteBack: hostWriteBack{host: d.host, store: d.store}, OnChange: d.events.onChange})
	if err != nil {
		return fail(err)
	}
	return &backend{
		admitter: ownerAdmitter{d.tab},
		executor: hostExecutor{host: d.host, grace: cfg.ReplyGrace.Duration, canary: front.ActorExecutor{Host: canary, Grace: cfg.ReplyGrace.Duration}},
		ready:    d.readyCheck,
		pending:  d.unreconciled,
		metrics:  d.register,
		routes: func(mux *http.ServeMux, srv *front.Server) {
			(&control{d: d, srv: srv, token: sec.ControlToken, sem: make(chan struct{}, cfg.LifecycleConcurrency)}).routes(mux)
		},
		background: []func(context.Context){d.reconcile, d.maintain, d.runProbe},
		close:      d.close,
		abort:      func() { d.host.Close(); canary.Close(); d.closePlacement() },
		breakers:   d.breakers(), // the admin endpoints' breakers, by name: "s3", and "placement" with Postgres
		host:       d.host,
	}, nil
}

// openPlacement opens the configured authority: the single-node file adapter, or Postgres (CP-4). The
// Postgres pool connects lazily, so the authority being down does not stop startup: keys stay unreconciled
// (unavailable) until it answers (Release It H1). Its breaker is "placement" in the metrics.
func (d *durableAgent) openPlacement(sec *Secrets) (owner.Placement, error) {
	c := d.cfg.PlacementPG
	if c == nil {
		return file.Open(d.cfg.PlacementDir)
	}
	if sec.PGDSN == "" {
		return nil, errs.Wrap(module, nil, "secrets_file: pg_dsn is required with placement_pg")
	}
	bc := c.Breaker.config()
	bc.OnChange = logx.BreakerLog(d.log, "placement")
	p, err := pg.Open(pg.Config{DSN: sec.PGDSN, AppName: "agentd/" + d.cfg.VM, Schema: c.Schema, MaxConns: c.MaxConns, Breaker: bc})
	if err != nil {
		return nil, err
	}
	d.pg = p
	return p, nil
}

// closePlacement closes the Postgres pool, if any (after the host: nothing calls the authority any more).
func (d *durableAgent) closePlacement() {
	if d.pg != nil {
		d.pg.Close()
	}
}

// breakers are the remote dependencies' breakers by name: "s3", and "placement" with Postgres.
func (d *durableAgent) breakers() map[string]*breaker.Breaker {
	m := map[string]*breaker.Breaker{"s3": d.s3b}
	if d.pg != nil {
		m["placement"] = d.pg.Breaker()
	}
	return m
}

// readyCheck passes once the first reconcile round finished: every key was tried once (Release It H1).
func (d *durableAgent) readyCheck(context.Context) error {
	if !d.firstRound.Load() {
		return errors.New("startup: first reconcile round running")
	}
	return nil
}

// unreconciled counts the keys still closed since restart (O(keys), for a scrape).
func (d *durableAgent) unreconciled() int {
	n := 0
	for _, k := range d.tab.Keys() {
		if d.tab.State(k).State == owner.Closed {
			n++
		}
	}
	return n
}

// reconcile is the startup job: reconcile every key loaded closed, retrying with backoff until all are done.
// Each key serves the moment its own reconcile succeeds.
func (d *durableAgent) reconcile(ctx context.Context) {
	report := reconcileLog(d.log)
	_ = d.tab.ReconcileUntilDone(ctx, owner.ReconcileOptions{Workers: d.cfg.StartupWorkers, PerKey: d.cfg.ReconcilePerKey.Duration},
		func(r owner.ReconcileReport, err error) {
			report(r, err)
			d.firstRound.Store(true)
		})
}

// maintain runs the host's periodic write-back and eviction until ctx ends.
func (d *durableAgent) maintain(ctx context.Context) {
	_ = d.host.RunMaintenance(ctx, d.cfg.Maintenance.Interval.Duration, d.ticks.report)
}

// close is the host.close shutdown phase: write back oldest-dirty first within the budget, keep the rest
// locally, and log what happened (VACTOR.SHUTDOWN; Release It H3). Then the canary.
func (d *durableAgent) close(ctx context.Context) error {
	rep, err := d.host.Shutdown(ctx)
	attrs := []any{"event", "VACTOR.SHUTDOWN", "written_back", rep.WrittenBack, "dropped", rep.Dropped, "kept", len(rep.Kept),
		"busy", rep.Busy, "took_ms", rep.Took.Milliseconds()}
	if err != nil {
		d.log.Warn("host closed with databases not written back", append(attrs, "kept_keys", rep.Kept, "cache_dir", rep.CacheDir)...)
	} else {
		d.log.Info("host closed", attrs...)
	}
	err = errors.Join(err, d.canary.Shutdown(ctx))
	d.closePlacement()
	return err
}

// register adds durable mode's metric families: S3 requests, the S3 breaker, the host, ownership transitions
// and the S3 probe (Release It O1).
func (d *durableAgent) register(reg *metrics.Registry) {
	s3Metrics(reg, d.s3.Metrics)
	metrics.Breakers(reg, d.breakers())
	vactorMetrics(reg, d.host, d.ticks.lastTick)
	d.events.register(reg)
	d.probe.register(reg)
}

// probeKey is the key the S3 probe reads: it has no manifest, so a healthy S3 answers "no manifest".
const probeKey = "~probe"

// s3Probe is the S3 canary: one manifest read every ready_interval, past the breaker, bounded by
// ready_timeout. Metrics only: it never fails readiness (Release It H2, C5). Transitions are logged.
type s3Probe struct {
	ok       atomic.Bool
	lastOK   atomic.Int64 // UnixNano; 0 = never
	failures atomic.Int64
}

// runProbe first makes sure the bucket exists (off the startup path: S3 being down must not delay serving),
// then probes every ready_interval.
func (d *durableAgent) runProbe(ctx context.Context) {
	bctx, cancel := context.WithTimeout(ctx, openTimeout)
	if err := d.s3.EnsureBucket(bctx); err != nil && ctx.Err() == nil {
		d.log.Warn("S3 bucket check failed; nothing activates from S3 until it answers", "event", "S3.BUCKET_CHECK_FAILED",
			"bucket", d.cfg.S3.Bucket, "error", err.Error())
	}
	cancel()
	t := time.NewTicker(d.cfg.ReadyInterval.Duration)
	defer t.Stop()
	for {
		d.probeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *durableAgent) probeOnce(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, d.cfg.ReadyTimeout.Duration)
	_, _, err := d.raw.ReadManifest(pctx, probeKey)
	cancel()
	if ctx.Err() != nil {
		return // stopping: says nothing about S3
	}
	ok := err == nil || errors.Is(err, durable.ErrNoManifest)
	if ok {
		d.probe.lastOK.Store(time.Now().UnixNano())
	} else {
		d.probe.failures.Add(1)
	}
	if d.probe.ok.Swap(ok) != ok {
		if ok {
			d.log.Info("S3 probe passes", "event", "S3.PROBE_OK")
		} else {
			d.log.Warn("S3 probe fails", "event", "S3.PROBE_FAILED", "error", err.Error())
		}
	}
}

func (p *s3Probe) register(reg *metrics.Registry) {
	reg.Add("tsdb_s3_probe_ok", "1 when the last S3 probe (a manifest read past the breaker) answered.", metrics.Gauge, func(e *metrics.Emit) {
		v := 0.0
		if p.ok.Load() {
			v = 1
		}
		e.Value(nil, v)
	})
	reg.Add("tsdb_s3_probe_failures_total", "S3 probes that got no answer.", metrics.Counter, func(e *metrics.Emit) {
		e.Value(nil, float64(p.failures.Load()))
	})
	reg.Add("tsdb_s3_probe_last_ok_timestamp_seconds", "When an S3 probe last answered (0 = never).", metrics.Gauge, func(e *metrics.Emit) {
		e.Value(nil, float64(p.lastOK.Load())/1e9)
	})
}
