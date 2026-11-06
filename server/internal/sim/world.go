package sim

import (
	"container/heap"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing/synctest"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/fsx"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

// Result is one run.
type Result struct {
	Seed       uint64
	Settings   string // every knob the seed decided (the trace's first line)
	Hash       string // SHA-256 of the trace: equal for two runs of one seed
	Trace      []string
	Violations []Violation
	Err        error // the run could not be set up or torn down (not a property violation)
	Stats      Stats
}

// Stats count what a run did.
type Stats struct {
	Steps, Requests, Acked, Crashes, Storms, Faults int
	LostInRPO                                       int      // acknowledged writes a crash lost inside the RPO
	Internal                                        []string // causes of internal answers (a bug, or a dependency failure mislabelled)
}

// Run runs one seed and returns what happened. live, if set, gets every trace line as the run goes.
//
//	pre: called inside a testing/synctest bubble, with no other goroutines of the bubble running
func Run(cfg Config, live io.Writer) Result {
	s := cfg.settle()
	t0 := time.Now() // the bubble's clock: the same instant on every run
	w := &world{s: s, clk: dettest.NewClock(t0), t0: t0, health: newHealth(), store: durabletest.NewStore(),
		place: ownertest.NewPlacement(), ownerDir: filepath.Join(s.Dir, "owner"),
		stormr: stream(s.Seed, "storms"), crashr: stream(s.Seed, "crashes"), frand: map[string]*rand.Rand{}}
	w.tr = newTrace(t0, w.clk.Now, live)
	for i := range s.Keys {
		w.keys = append(w.keys, fmt.Sprintf("t:db%d", i))
	}
	w.model = newModel(t0, s.RPO, w.health, w.tr, w.keys)
	w.tr.add("sim", "%s", s)
	w.tr.flush() // the settings are the trace's first line
	res := Result{Seed: s.Seed, Settings: s.String()}
	if err := w.boot(); err != nil {
		res.Err = err
	} else {
		w.loop(t0.Add(s.Duration))
		res.Err = w.finish()
	}
	w.tr.flush()
	res.Hash, res.Trace, res.Violations = w.tr.hash(), w.tr.all(), w.model.violations()
	w.stats.LostInRPO = w.model.lostOK
	res.Stats = w.stats
	return res
}

// world is everything that outlives an incarnation: clock, store, placement, owner directory, clients,
// checker.
type world struct {
	s        settings
	clk      *dettest.Clock
	t0       time.Time
	tr       *trace
	model    *model
	health   *health
	store    *durabletest.Store
	place    *ownertest.Placement
	ownerDir string
	keys     []string

	stormr   *rand.Rand // storms: when, how long
	crashr   *rand.Rand // crashes: when, restart delay, torn temp files
	agenda   agenda
	faultsOn atomic.Bool
	fmu      sync.Mutex
	frand    map[string]*rand.Rand // per-key fault streams

	mu    sync.Mutex
	cur   *agent // nil while the process is down
	incs  int
	stats Stats

	stopClients context.CancelFunc
	clients     sync.WaitGroup
	teardowns   []<-chan struct{}
}

func (w *world) current() *agent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

func (w *world) count(f func(*Stats)) {
	w.mu.Lock()
	f(&w.stats)
	w.mu.Unlock()
}

// boot seeds every database (generation 1: the workload's empty database, epoch 1), creates the placement
// rows, starts incarnation 1 with first-boot claims, then the clients, faults and the event schedule.
func (w *world) boot() error {
	if err := os.MkdirAll(w.ownerDir, 0o755); err != nil {
		return err
	}
	w.store.UseClock(w.clk)
	w.store.SetCallFaults(w.callFaults)
	w.place.UseClock(w.clk)
	for _, k := range w.keys {
		data, err := w.s.Workload.Seed(w.s.Dir, k)
		if err != nil {
			return fmt.Errorf("sim: seed %s: %w", k, err)
		}
		w.store.Seed(k, epoch, data)
		w.place.Create(k, owner.Place{Owner: self, Epoch: epoch, State: owner.PlaceActive})
	}
	w.checkGenerations()
	a, err := w.newAgent(1, true)
	if err != nil {
		return err
	}
	w.cur, w.incs = a, 1
	w.faultsOn.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	w.stopClients = cancel
	for _, k := range w.keys {
		w.clients.Go(func() { w.client(ctx, k) })
	}
	f := w.s.faults
	if f.StormEvery > 0 {
		w.at(exp(w.stormr, f.StormEvery), "storm", w.storm)
	}
	if f.CrashEvery > 0 {
		w.at(exp(w.crashr, f.CrashEvery), "crash", w.crash)
	}
	return nil
}

// settle lets every goroutine run until it blocks, then checks the store and ends the trace step.
func (w *world) settle() {
	synctest.Wait()
	w.checkGenerations()
	w.tr.flush()
}

// loop is the event loop: settle; then fire the earliest timer alone, or run the earliest sim event (timers
// first on a tie); until end. The bubble's own clock is kept level with the simulated one, so a stray
// wall-clock wait (SQLite's busy handler, database/sql's timers) fires at the right simulated instant too.
func (w *world) loop(end time.Time) {
	for {
		w.settle()
		if len(w.model.violations()) > 0 && !w.s.KeepGoing {
			return
		}
		tc, okc := w.clk.Next()
		ev, oke := w.agenda.peek()
		timer := okc && (!oke || !tc.After(ev.at))
		var at time.Time
		switch {
		case timer:
			at = tc
		case oke:
			at = ev.at
		default:
			return // nothing will ever happen again
		}
		if at.After(end) {
			return
		}
		if d := at.Sub(time.Now()); d > 0 {
			time.Sleep(d)
			continue
		}
		w.count(func(s *Stats) { s.Steps++ })
		if timer {
			w.clk.Step()
			continue
		}
		heap.Pop(&w.agenda)
		w.clk.Advance(ev.at.Sub(w.clk.Now()))
		ev.fn()
	}
}

// finish stops the clients, crashes the process one last time (a final restore check), and steps the clock
// until every goroutine the run started has stopped.
func (w *world) finish() error {
	w.faultsOn.Store(false)
	w.stopClients()
	w.crash()
	done := make(chan struct{})
	go func() {
		w.clients.Wait()
		for _, t := range w.teardowns {
			<-t
		}
		close(done)
	}()
	for range 10_000 {
		w.settle()
		select {
		case <-done:
			return nil
		default:
		}
		if _, ok := w.clk.Step(); !ok {
			time.Sleep(time.Second) // only bubble timers left
		}
	}
	return fmt.Errorf("sim: the run's goroutines did not stop")
}

// checkGenerations has the checker look at every database's manifest (at quiescence: no call is half-way).
func (w *world) checkGenerations() {
	now := w.clk.Now()
	for _, k := range w.keys {
		m, ok := w.store.Manifest(k)
		if !ok {
			continue
		}
		w.model.generation(now, k, m, func() ([]string, error) { return w.marks(m) })
	}
}

// marks reads the generation m names, as a restarted process would start from it.
func (w *world) marks(m durable.Manifest) ([]string, error) {
	if m.Generation == 0 {
		return nil, nil
	}
	data, ok := w.store.Object(m.Object)
	if !ok {
		return nil, fmt.Errorf("object %s is missing", m.Object)
	}
	path := filepath.Join(w.s.Dir, "oracle.db")
	defer os.Remove(path)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, err
	}
	return w.s.Workload.Observe(path)
}

// crash kills the running incarnation, if any, and checks what a restart will start from.
func (w *world) crash() {
	w.mu.Lock()
	a := w.cur
	w.cur = nil
	w.mu.Unlock()
	if a == nil {
		return
	}
	now := w.clk.Now()
	w.teardowns = append(w.teardowns, a.kill())
	w.health.end("breaker", now) // the next process has a new breaker
	w.count(func(s *Stats) { s.Crashes++ })
	w.tr.add("sim", "crash: incarnation %d", a.n)
	for _, k := range w.keys {
		m, ok := w.store.Manifest(k)
		var marks []string
		var err error
		if ok {
			marks, err = w.marks(m)
		}
		if err != nil {
			w.model.mu.Lock()
			w.model.violate(now, k, "the manifest's generation %d cannot be read at the crash: %v", m.Generation, err)
			w.model.mu.Unlock()
			continue
		}
		w.model.restore(now, k, marks)
	}
	if w.crashr.IntN(3) == 0 { // a Put that crashed before its rename leaves a temp file (owner.FileStore drops it)
		name := filepath.Join(w.ownerDir, fmt.Sprintf("%storn-%d", fsx.TempPrefix, a.n))
		if err := os.WriteFile(name, []byte("{torn"), 0o644); err == nil {
			w.tr.add("sim", "torn temp file left in the owner directory")
		}
	}
	if f := w.s.faults; f.CrashEvery > 0 && w.faultsOn.Load() {
		w.at(uniform(w.crashr, f.RestartMax), "restart", w.restart)
	}
}

// restart starts the next incarnation from the owner directory and the store.
func (w *world) restart() {
	w.mu.Lock()
	w.incs++
	n := w.incs
	w.mu.Unlock()
	a, err := w.newAgent(n, false)
	if err != nil {
		w.model.mu.Lock()
		w.model.violate(w.clk.Now(), "-", "restart %d failed: %v", n, err)
		w.model.mu.Unlock()
		return
	}
	w.mu.Lock()
	w.cur = a
	w.mu.Unlock()
	w.tr.add("sim", "restart: incarnation %d", n)
	w.at(exp(w.crashr, w.s.faults.CrashEvery), "crash", w.crash)
}

// storm makes every store call fail for a while (S3 answering 503 to everything).
func (w *world) storm() {
	now := w.clk.Now()
	d := uniform(w.stormr, w.s.faults.StormMax) + time.Second
	w.store.SetFaults(durabletest.Faults{Down: true})
	w.health.begin("storm", now)
	w.count(func(s *Stats) { s.Storms++ })
	w.tr.add("sim", "storm for %v", d)
	w.at(d, "storm end", func() {
		w.store.SetFaults(durabletest.Faults{})
		w.health.end("storm", w.clk.Now())
		w.tr.add("sim", "storm over")
		w.at(exp(w.stormr, w.s.faults.StormEvery), "storm", w.storm)
	})
}

// callFaults decides one store call's faults from key's own stream (so calls of other databases, in whatever
// order they interleave, never move it). Every call draws the same four numbers.
func (w *world) callFaults(op, key string, f durabletest.Faults) durabletest.Faults {
	if !w.faultsOn.Load() {
		return f
	}
	w.fmu.Lock()
	r := w.frand[key]
	if r == nil {
		r = stream(w.s.Seed, "faults/"+key)
		w.frand[key] = r
	}
	stall, fail, lose, n := r.Float64(), r.Float64(), r.Float64(), r.Uint64()
	w.fmu.Unlock()
	p := w.s.faults
	var what []string
	if stall < p.Stall && p.StallMax > 0 {
		f.Delay = 50*time.Millisecond + time.Duration(n%uint64(p.StallMax))
		what = append(what, "stall "+f.Delay.String())
	}
	switch {
	case lose < p.LoseReply && op == durable.OpPublish && n%2 == 0:
		f.LosePublishReply, what = true, append(what, "lose-reply")
	case lose < p.LoseReply && op == durable.OpPublish:
		f.ApplyThenStale, what = true, append(what, "apply-then-412")
	case lose < p.LoseReply && op == durable.OpPutGen:
		f.LoseUploadReply, what = true, append(what, "lose-reply")
	case fail < p.FailCall:
		switch op {
		case durable.OpGetManifest:
			f.FailRead = true
		case durable.OpPutGen:
			f.FailUpload = true
		case durable.OpFetch:
			f.FailFetch = true
		case durable.OpPublish:
			f.FailPublish = true
		case durable.OpDelete:
			f.FailDelete = true
		default:
			f.Down = true
		}
		what = append(what, "fail")
	}
	if len(what) > 0 {
		now := w.clk.Now()
		w.health.mark(now, now.Add(f.Delay))
		w.count(func(s *Stats) { s.Faults++ })
		w.tr.add("fault/"+key, "%s %s", op, strings.Join(what, " "))
	}
	return f
}

// client is key's one sequential client: think, pick a request, send it with retries, repeat.
func (w *world) client(ctx context.Context, key string) {
	r := stream(w.s.Seed, "client/"+key)
	ids := det.NewIDs("r", det.NewRand(subSeed(w.s.Seed, "ids/"+key)))
	m := w.s.mix
	var reuse string // a request id whose write failed: the next write may reuse it
	for {
		if det.Sleep(ctx, w.clk, time.Duration(r.ExpFloat64()*float64(m.Think))) != nil {
			return
		}
		req := Request{Value: r.Int64N(1000)}
		switch x := r.Float64() * (m.Write + m.BadWrite + m.Read); {
		case x < m.Write:
			req.Kind, req.ID = Write, ids.NewID()
			if reuse != "" && r.Float64() < m.Reuse {
				req.ID = reuse
			}
			reuse = ""
		case x < m.Write+m.BadWrite:
			req.Kind, req.ID = BadWrite, ids.NewID()
		default:
			req.Kind = Read
		}
		if a := w.request(ctx, r, key, req); req.Kind == BadWrite && a.Code == front.BadRequest {
			reuse = req.ID
		}
	}
}

const maxTries = 8

// request sends req until a final answer the client sees (ok, or bad_request), or maxTries, like the real
// client: the same request id every time, backoff with jitter, at least the agent's Retry-After.
func (w *world) request(ctx context.Context, r *rand.Rand, key string, req Request) Answer {
	for try := 1; ; try++ {
		if req.Kind != Read {
			w.model.sent(key, req.ID)
		}
		var ans Answer
		alive := true
		if a := w.current(); a == nil {
			ans = Answer{Code: front.Unavailable} // connection refused: nothing ran
		} else {
			ans = w.s.Workload.Send(a.ctx, a, key, req)
			alive = a.g.alive()
		}
		w.model.answer(w.clk.Now(), key, req, ans, alive)
		w.count(func(s *Stats) {
			s.Requests++
			if ans.Code == front.OK && alive {
				s.Acked++
			}
			if ans.Code == front.Internal && ans.Err != nil {
				s.Internal = append(s.Internal, ans.Err.Error())
			}
		})
		clientLost := r.Float64() < w.s.faults.ClientLose || !alive
		note := ""
		switch {
		case !alive:
			note = " (process died: answer lost)"
		case clientLost:
			note = " (answer lost on the way)"
		}
		w.tr.add("c/"+key, "%s %s try %d -> %s%s%s%s", req.Kind, req.ID, try, ans.Code, flag(ans.Duplicate, " dup"), flag(ans.Ambiguous, " ambiguous"), note)
		if clientLost {
			ans = Answer{Code: front.Expired, Ambiguous: req.Kind != Read}
		}
		if ans.Code == front.OK || ans.Code == front.BadRequest || try == maxTries || ctx.Err() != nil {
			return ans
		}
		d := max(ans.RetryAfter, 100*time.Millisecond<<min(try-1, 6))
		d = d/2 + time.Duration(r.Int64N(int64(d/2)+1))
		if det.Sleep(ctx, w.clk, d) != nil {
			return ans
		}
	}
}

func flag(b bool, s string) string {
	if b {
		return s
	}
	return ""
}

func exp(r *rand.Rand, mean time.Duration) time.Duration {
	return time.Duration(r.ExpFloat64() * float64(mean))
}

func uniform(r *rand.Rand, upTo time.Duration) time.Duration {
	if upTo <= 0 {
		return 0
	}
	return time.Duration(r.Int64N(int64(upTo)))
}

// at schedules fn d from now on the sim's agenda.
func (w *world) at(d time.Duration, name string, fn func()) {
	w.agenda.seq++
	heap.Push(&w.agenda, event{at: w.clk.Now().Add(d), seq: w.agenda.seq, name: name, fn: fn})
}

type event struct {
	at   time.Time
	seq  uint64
	name string
	fn   func()
}

// agenda is the sim's own events (crash, restart, storm), earliest first, then in scheduling order.
type agenda struct {
	evs []event
	seq uint64
}

func (a *agenda) Len() int { return len(a.evs) }
func (a *agenda) Less(i, j int) bool {
	if !a.evs[i].at.Equal(a.evs[j].at) {
		return a.evs[i].at.Before(a.evs[j].at)
	}
	return a.evs[i].seq < a.evs[j].seq
}
func (a *agenda) Swap(i, j int) { a.evs[i], a.evs[j] = a.evs[j], a.evs[i] }
func (a *agenda) Push(x any)    { a.evs = append(a.evs, x.(event)) }
func (a *agenda) Pop() any {
	e := a.evs[len(a.evs)-1]
	a.evs = a.evs[:len(a.evs)-1]
	return e
}

func (a *agenda) peek() (event, bool) {
	if len(a.evs) == 0 {
		return event{}, false
	}
	return a.evs[0], true
}
