package bench

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/load"
	"github.com/nickstrad/ts-db-go/internal/panicx"
	"github.com/nickstrad/ts-db-go/internal/retry"
	"github.com/nickstrad/ts-db-go/internal/schema"
	"github.com/nickstrad/ts-db-go/internal/stats"
	"github.com/nickstrad/ts-db-go/internal/workload"
)

const module = "bench"

// Summary is what a finished run reports.
type Summary struct {
	OutDir                        string
	Attempts                      int64 // answered attempts (first tries and retries)
	Written, Dropped              int64 // records
	RetriesScheduled, RetriesLost int64 // lost = found every worker busy when its backoff ended
	Clients                       []ClientSummary
}

type ClientSummary struct {
	Name            string
	Firsts, Retries int64
}

// NoFileLimit is the open-file soft limit (0 if unknown).
func NoFileLimit() uint64 {
	var rl syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) != nil {
		return 0
	}
	return rl.Cur
}

// Run executes cfg (normalized and resolved; see Resolve) and writes its files to cfg.OutDir. Cancelling ctx stops
// sending new requests; what is in flight finishes and every file is still written, and Run returns an error
// wrapping ctx.Err(). A panic in any goroutine Run starts is re-raised in the caller's goroutine.
func Run(ctx context.Context, cfg *load.Config, log *slog.Logger) (Summary, error) {
	nofile := NoFileLimit()
	for _, line := range Plan(cfg, nofile) {
		log.Info(line)
	}
	catalog := schema.NewCatalog(cfg.Seed, cfg.Schema)
	if err := prepareDBDir(cfg.DBDir, catalog.Fingerprint(), log); err != nil {
		return Summary{}, err
	}
	if err := writeRunJSON(cfg, catalog.Fingerprint(), nofile); err != nil {
		return Summary{}, err
	}
	r, err := newRunner(ctx, cfg, log)
	if err != nil {
		return Summary{}, err
	}
	defer r.cancel()
	defer r.closeFiles()
	return r.run(catalog)
}

// runner is one run's shared state.
type runner struct {
	ctx      context.Context
	cancel   context.CancelFunc
	cfg      *load.Config
	actorCfg actor.Config // cfg.Actor with defaults, built once
	log      *slog.Logger
	start    time.Time
	dir      *actor.Directory

	requests        *stats.RequestWriter
	phases, schemas *stats.CSV
	sampler         *stats.Sampler

	policies []retry.Policy
	budgets  []*retry.Budget
	jobs     chan job

	// outstanding counts jobs queued, running, or waiting out a backoff. The run drains when it reaches 0.
	outstanding atomic.Int64
	stopping    atomic.Bool // no new retries
	recorded    atomic.Int64

	inFlight, completed atomic.Int64
	phaseNow, dbsNow    atomic.Int64
	targetQPS           atomic.Uint64 // float64 bits
	retriesLost         atomic.Int64

	backoffMu sync.Mutex
	backoffs  map[*backoff]struct{} // retries waiting out their delay
	lost      *stats.Buffer         // records of retries that were never sent (guarded by backoffMu)

	crash crash
}

func newRunner(ctx context.Context, cfg *load.Config, log *slog.Logger) (_ *runner, err error) {
	acfg := cfg.Actor.Actor()
	r := &runner{cfg: cfg, actorCfg: acfg, log: log, start: time.Now(), dir: actor.NewDirectory(acfg),
		jobs: make(chan job, queueSize(cfg)), backoffs: map[*backoff]struct{}{}}
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.crash.cancel = r.cancel
	for _, cp := range cfg.Clients {
		p := cp.Policy()
		r.policies = append(r.policies, p)
		r.budgets = append(r.budgets, &retry.Budget{Ratio: p.BudgetRatio})
	}
	defer func() {
		if err != nil {
			if r.requests != nil {
				r.requests.Close()
			}
			r.closeFiles()
			r.cancel()
		}
	}()
	out := func(name string) string { return filepath.Join(cfg.OutDir, name) }
	format := stats.RecordFormat{Clients: cfg.ClientNames(), Classes: make([]string, len(workload.Classes))}
	for i, c := range workload.Classes {
		format.Classes[i] = string(c)
	}
	if r.requests, err = stats.NewRequestWriter(out("requests.csv"), format); err != nil {
		return nil, err
	}
	r.lost = r.requests.NewBuffer()
	if r.phases, err = stats.NewCSV(out("phases.csv"), phaseColumns...); err != nil {
		return nil, err
	}
	if r.schemas, err = stats.NewCSV(out("schemas.csv"), schemaColumns...); err != nil {
		return nil, err
	}
	r.phaseNow.Store(-1)
	r.sampler, err = stats.NewSampler(out("samples.csv"), cfg.SampleEvery.Duration, r.start, stats.Gauges{
		Phase:     func() int { return int(r.phaseNow.Load()) },
		DBs:       func() int { return int(r.dbsNow.Load()) },
		TargetQPS: func() float64 { return math.Float64frombits(r.targetQPS.Load()) },
		Actors:    func() (int, int, int) { s := r.dir.Stats(); return s.Open, s.Queued, s.MaxMailbox },
		InFlight:  r.inFlight.Load,
		Completed: r.completed.Load,
		Dropped:   r.requests.Dropped,
	})
	return r, err
}

// closeFiles closes the small CSVs and the actors; safe after a partial newRunner and safe to repeat.
func (r *runner) closeFiles() {
	for _, c := range []*stats.CSV{r.phases, r.schemas} {
		if c != nil {
			c.Close()
		}
	}
	r.dir.Close()
}

func (r *runner) run(catalog *schema.Catalog) (Summary, error) {
	cfg := r.cfg
	r.sampler.Start()
	var workers sync.WaitGroup
	for w := range cfg.Workers {
		workers.Go(func() { r.worker(w) })
	}
	gens := load.NewGenerators(catalog, cfg.Workload)
	rng := rand.New(rand.NewPCG(cfg.Seed, 0xC0FFEE))
	genBuf := r.requests.NewBuffer() // the dispatcher's records (requests it could not send)

	var runErr error
	for i := range cfg.Phases {
		if runErr = r.runPhase(i, catalog, gens, rng, genBuf); runErr != nil {
			break
		}
	}

	// Drain: wait for every queued, running or backing-off job. When stopped early, no new retries start and
	// the pending backoffs are cancelled (recorded as never sent) instead of waited out.
	if runErr != nil {
		r.stopping.Store(true)
		r.cancelBackoffs()
	}
	for r.outstanding.Load() > 0 && !r.crash.happened() {
		time.Sleep(10 * time.Millisecond)
	}
	r.stopping.Store(true)
	close(r.jobs)
	workers.Wait()
	r.crash.repanic()

	genBuf.Flush()
	r.backoffMu.Lock()
	r.lost.Flush()
	r.backoffMu.Unlock()
	r.targetQPS.Store(0)
	sampleErr := r.sampler.Stop()
	if err := r.requests.Close(); err != nil && runErr == nil {
		runErr = err
	}
	assert.That(r.requests.Written()+r.requests.Dropped() == r.recorded.Load(),
		"records: %d written + %d dropped != %d recorded", r.requests.Written(), r.requests.Dropped(), r.recorded.Load())
	r.dir.Close()
	if !cfg.KeepDBs {
		os.RemoveAll(cfg.DBDir)
	}

	sum := Summary{OutDir: cfg.OutDir, Attempts: r.completed.Load(), Written: r.requests.Written(),
		Dropped: r.requests.Dropped(), RetriesLost: r.retriesLost.Load()}
	for i, b := range r.budgets {
		f, n := b.Counts() // a retry counts here once the budget allows it, i.e. once it is scheduled
		sum.RetriesScheduled += n
		sum.Clients = append(sum.Clients, ClientSummary{Name: cfg.Clients[i].Name, Firsts: f, Retries: n})
	}
	if runErr == nil && sampleErr != nil {
		runErr = sampleErr
	}
	return sum, runErr
}

// runPhase grows the directory to the phase's size, then sends requests at the phase's rate until it ends or
// the run is cancelled.
func (r *runner) runPhase(pi int, catalog *schema.Catalog, gens *load.Generators, rng *rand.Rand, genBuf *stats.Buffer) error {
	cfg, p := r.cfg, &r.cfg.Phases[pi]
	r.phaseNow.Store(int64(pi))
	r.targetQPS.Store(0)
	st := phaseStats{index: pi, setupStart: time.Since(r.start)}

	before := r.dir.Len()
	catalog.Ensure(p.DBs)
	var err error
	st.grow, err = r.dir.Grow(p.DBs, func(i int) string { return DBPath(cfg.DBDir, i) },
		func(i int) error { return r.seedDB(DBPath(cfg.DBDir, i), catalog.Get(i)) }, cfg.SetupWorkers)
	if err != nil {
		return errs.Wrap(module, err, "phase %q: cannot grow to %d databases; no requests were sent in this phase", p.Name, p.DBs)
	}
	for i := before; i < r.dir.Len(); i++ {
		for _, row := range schemaRows(catalog.Get(i)) {
			if err := r.schemas.Row(row...); err != nil {
				return err
			}
		}
	}
	r.dbsNow.Store(int64(p.DBs))
	if st.grow.Added > 0 {
		r.log.Info("databases added", "phase", p.Name, "added", st.grow.Added, "seed", st.grow.Seed.Round(time.Millisecond),
			"open", st.grow.Open.Round(time.Millisecond), "open_now", r.dir.Stats().Open)
	}
	picker := load.NewPicker(rng, gens, cfg, p)
	r.log.Info("phase start", "phase", pi, "name", p.Name, "dbs", p.DBs, "start_qps", p.StartQPS, "end_qps", p.EndQPS,
		"shape", p.Shape, "duration", p.Duration.Duration)

	t0 := time.Now()
	st.start = t0.Sub(r.start)
	pacer := load.NewPacer(p, t0)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for done := false; !done; {
		var now time.Time
		select {
		case <-r.ctx.Done():
			return errs.Wrap(module, r.ctx.Err(), "stopped during phase %q; requests in flight were finished and recorded", p.Name)
		case now = <-tick.C:
		}
		var n int
		var rate float64
		n, rate, done = pacer.Due(now)
		r.targetQPS.Store(math.Float64bits(rate))
		for range n {
			pk := picker.Next()
			r.budgets[pk.Client].First()
			j := newJob(pi, pk, now)
			if r.enqueue(j) {
				st.sent++
				continue
			}
			st.notSent++
			if pk.Record {
				r.record(genBuf, notSent(j, r.start))
			}
		}
	}
	st.end = time.Since(r.start)
	if err := r.phases.Row(st.row(p)...); err != nil {
		return err
	}
	r.log.Info("phase end", "phase", pi, "name", p.Name, "sent", st.sent, "gen_full", st.notSent, "completed", r.completed.Load())
	return nil
}

// enqueue hands j to a worker without blocking and reports whether it did. j counts as outstanding until a
// worker finishes it.
func (r *runner) enqueue(j job) bool {
	r.outstanding.Add(1)
	select {
	case r.jobs <- j:
		return true
	default:
		r.outstanding.Add(-1)
		return false
	}
}

func (r *runner) record(b *stats.Buffer, rec stats.Record) {
	r.recorded.Add(1)
	b.Add(rec)
}

// worker sends one attempt at a time, records it, and schedules any retry.
func (r *runner) worker(w int) {
	defer r.crash.guard()
	buf := r.requests.NewBuffer()
	defer buf.Flush()
	rng := rand.New(rand.NewPCG(r.cfg.Seed, uint64(w)+1000))
	for j := range r.jobs {
		r.inFlight.Add(1)
		res, send, done := r.send(j)
		r.inFlight.Add(-1)
		r.completed.Add(1)
		rec := answered(j, res, send, done, r.start)
		rec.Final = !r.maybeRetry(j, rec, retry.HintOf(res.Err), rng, done)
		if j.pick.Record {
			r.record(buf, rec)
		}
		r.outstanding.Add(-1)
	}
}

// send submits j's statement to its database and waits for the reply.
func (r *runner) send(j job) (res actor.Result, send, done time.Time) {
	req := &actor.Request{SQL: j.pick.SQL, Args: j.pick.Args, Write: j.pick.Write,
		StmtTimeout: r.cfg.StatementTimeout.Duration, Deadline: j.sched.Add(r.cfg.RequestTimeout.Duration)}
	send = time.Now()
	if err := r.dir.Get(j.pick.DB).Submit(req); err != nil {
		res.Err = err
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), replyWait(r.cfg))
		res = req.Wait(ctx)
		cancel()
	}
	return res, send, time.Now()
}

// maybeRetry schedules the next attempt if the client's policy and budget allow it, and reports whether it did.
// hint is the answer's retry hint (retry.HintOf): actor errors carry none today, so it is 0 until one does.
func (r *runner) maybeRetry(j job, rec stats.Record, hint time.Duration, rng *rand.Rand, now time.Time) bool {
	if r.stopping.Load() {
		return false
	}
	delay, ok := r.policies[j.pick.Client].Decide(rng, retry.Attempt{
		N: int(j.attempt), Outcome: rec.Outcome, Elapsed: now.Sub(j.first), Prev: j.prevDelay, ServerHint: hint})
	if !ok || !r.budgets[j.pick.Client].TryRetry() {
		return false
	}
	next := retryOf(j, delay)
	if delay == 0 { // no backoff: straight back in the queue, no timer
		next.sched = now
		if !r.enqueue(next) {
			r.loseRetry(next)
		}
		return true
	}
	b := &backoff{j: next}
	r.outstanding.Add(1) // released when the backoff fires or is cancelled
	r.backoffMu.Lock()
	r.backoffs[b] = struct{}{}
	b.t = time.AfterFunc(delay, func() { r.fireBackoff(b) })
	r.backoffMu.Unlock()
	return true
}

// backoff is a retry waiting out its delay.
type backoff struct {
	t *time.Timer
	j job
}

func (r *runner) fireBackoff(b *backoff) {
	defer r.crash.guard()
	r.backoffMu.Lock()
	_, pending := r.backoffs[b]
	delete(r.backoffs, b)
	r.backoffMu.Unlock()
	if !pending { // cancelled
		return
	}
	defer r.outstanding.Add(-1)
	b.j.sched = time.Now()
	if r.stopping.Load() || !r.enqueue(b.j) {
		r.loseRetry(b.j)
	}
}

// cancelBackoffs records every pending retry as never sent. Membership in r.backoffs is ownership: a timer that
// already fired but has not taken the lock finds itself removed and does nothing.
func (r *runner) cancelBackoffs() {
	r.backoffMu.Lock()
	cancelled := make([]*backoff, 0, len(r.backoffs))
	for b := range r.backoffs {
		b.t.Stop()
		cancelled = append(cancelled, b)
		delete(r.backoffs, b)
	}
	r.backoffMu.Unlock()
	now := time.Now()
	for _, b := range cancelled {
		b.j.sched = now
		r.loseRetry(b.j)
		r.outstanding.Add(-1)
	}
}

// loseRetry records a scheduled retry that was never sent (queue full, or the run stopped).
func (r *runner) loseRetry(j job) {
	r.retriesLost.Add(1)
	if j.pick.Record {
		r.backoffMu.Lock()
		r.record(r.lost, notSent(j, r.start))
		r.backoffMu.Unlock()
	}
}

// crash keeps the first panic from any goroutine of the run and cancels the run; repanic re-raises it in the
// caller's goroutine, with the stack of the goroutine that panicked.
type crash struct {
	mu     sync.Mutex
	value  *panicx.Panic
	cancel context.CancelFunc
}

func (c *crash) guard() {
	p := panicx.From(recover())
	if p == nil {
		return
	}
	c.mu.Lock()
	if c.value == nil {
		c.value = p
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *crash) happened() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.value != nil }

func (c *crash) repanic() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != nil {
		panic(c.value)
	}
}

// seedDB creates database path from its schema unless it exists (a kept directory). actor.Seed is atomic, so
// an existing file is always complete.
func (r *runner) seedDB(path string, s *schema.Schema) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return actor.Seed(path, r.actorCfg, s.DDL(), func(exec func(string, ...any) error) error {
		return s.Fill(r.cfg.Seed, exec)
	})
}

// prepareDBDir makes dir hold databases built from fingerprint, clearing it if it was built from another.
func prepareDBDir(dir, fingerprint string, log *slog.Logger) error {
	marker := filepath.Join(dir, "FINGERPRINT")
	if b, err := os.ReadFile(marker); err == nil && string(b) != fingerprint {
		log.Info("database dir was built from another schema config; recreating", "dir", dir)
		if err := os.RemoveAll(dir); err != nil {
			return errs.Wrap(module, err, "cannot clear database dir").With("dir", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(module, err, "cannot create database dir").With("dir", dir)
	}
	if err := os.WriteFile(marker, []byte(fingerprint), 0o644); err != nil {
		return errs.Wrap(module, err, "cannot write database dir marker").With("dir", dir)
	}
	return nil
}

func writeRunJSON(cfg *load.Config, fingerprint string, nofile uint64) error {
	b, err := json.MarshalIndent(map[string]any{
		"config":             cfg,
		"schema_fingerprint": fingerprint,
		"started":            time.Now().UTC().Format(time.RFC3339),
		"go":                 runtime.Version(),
		"goos":               runtime.GOOS,
		"goarch":             runtime.GOARCH,
		"num_cpu":            runtime.NumCPU(),
		"gomaxprocs":         runtime.GOMAXPROCS(0),
		"nofile_soft":        nofile,
		"driver":             "modernc.org/sqlite (pure Go)",
	}, "", "  ")
	if err != nil {
		return errs.Wrap(module, err, "cannot encode run.json")
	}
	if err := os.WriteFile(filepath.Join(cfg.OutDir, "run.json"), b, 0o644); err != nil {
		return errs.Wrap(module, err, "cannot write run.json").With("dir", cfg.OutDir)
	}
	return nil
}
