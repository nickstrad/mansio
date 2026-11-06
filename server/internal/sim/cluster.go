package sim

import (
	"container/heap"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing/synctest"
	"time"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/edge"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
	"github.com/nickstrad/mansio/server/internal/ratelimit"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/ticket/tickettest"
)

// ClusterResult is one cluster run.
type ClusterResult struct {
	Seed       uint64
	Settings   string
	Hash       string // SHA-256 of the trace: equal for two runs of one config
	Trace      []string
	Violations []Violation
	Err        error // the run could not be set up or torn down
	Stats      ClusterStats
	// Script is the nemesis schedule the run drew (or was given): a ClusterConfig with it replays the same
	// operations, and a shrink cuts it down.
	Script []Op
	// Config is the run's config with every knob explicit (what a regression vector stores).
	Config ClusterConfig
}

// ClusterStats count what a run did.
type ClusterStats struct {
	Steps, Requests, Acked, Kills, Restarts, Attests, Moves, MovesDone, Creates, Partitions int
	LostInRPO                                                                               int
	Answers                                                                                 map[string]int // client-side answers by tag
	Ops                                                                                     int            // linearizability: operations checked
	Internal                                                                                []string
}

// Op is one nemesis operation of a run's script.
type Op struct {
	At   time.Duration `json:"at"`             // since the start
	Kind string        `json:"kind"`           // partition, kill, restart, attest, move, create, cacheloss, storm, pgdown
	Node string        `json:"node,omitempty"` // the VM, database or node it acts on
	Cut  []string      `json:"cut,omitempty"`  // partition: the nodes cut off from Node
	For  time.Duration `json:"for,omitempty"`  // partition, storm, pgdown: how long
}

func (o Op) String() string {
	s := fmt.Sprintf("%v %s %s", o.At, o.Kind, o.Node)
	if len(o.Cut) > 0 {
		s += " from " + strings.Join(o.Cut, ",")
	}
	if o.For > 0 {
		s += fmt.Sprintf(" for %v", o.For)
	}
	return s
}

const (
	tenant    = "t"
	maxDBs    = 8 // databases a run may have (boot + creates)
	nRegs     = 3
	nAccts    = 4
	balance0  = 100
	opTimeout = 30 * time.Second // one controller operation
)

// RunCluster runs one cluster config and returns what happened. live, if set, gets every trace line.
//
//	pre: called inside a testing/synctest bubble, with no other goroutines of the bubble running
func RunCluster(cfg ClusterConfig, live io.Writer) ClusterResult {
	s := cfg.settle()
	t0 := time.Now()
	w := &cworld{s: s, clk: dettest.NewClock(t0), t0: t0, health: newHealth(), store: durabletest.NewStore(),
		place: ownertest.NewPlacement(), byVM: map[string]*cvm{}, frand: map[string]*rand.Rand{}, dbs: map[string]*cdb{}}
	w.tr = newTrace(t0, w.clk.Now, live)
	w.net = newNet(w)
	for i := range s.VMs {
		vm := fmt.Sprintf("vm%d", i+1)
		w.vms = append(w.vms, vm)
		w.byVM[vm] = &cvm{name: vm, ownerDir: filepath.Join(s.Dir, vm, "owner")}
	}
	w.model = newCModel(w)
	w.tr.add("sim", "%s", s)
	w.tr.flush()
	res := ClusterResult{Seed: s.Seed, Settings: s.String(), Config: s.explicit()}
	if err := w.boot(); err != nil {
		res.Err = err
	} else {
		w.loop(t0.Add(s.Duration))
		res.Err = w.finish()
	}
	w.model.final()
	w.tr.flush()
	res.Hash, res.Trace, res.Violations = w.tr.hash(), w.tr.all(), w.model.violations()
	w.mu.Lock()
	res.Stats, res.Script = w.stats, slices.Clone(w.script)
	w.mu.Unlock()
	res.Stats.LostInRPO, res.Stats.Ops = w.model.lostOK, w.model.checked
	return res
}

// cworld is a cluster run: the clock, the network, the shared store and authority, controld, the edge, the
// VMs, the databases and their clients, and the checker.
type cworld struct {
	s      clusterSettings
	clk    *dettest.Clock
	t0     time.Time
	tr     *trace
	model  *cmodel
	health *health
	net    *netw
	store  *durabletest.Store
	place  *ownertest.Placement
	pgDown atomic.Bool
	doors  doors
	seq    atomic.Int64 // request arrival numbers (doors)

	// controld
	reg       *registry
	loc       *locator.Locator
	ctl       *control.Controller
	signer    *ticket.Signer
	keys      *tickettest.MemKeys
	apiKey    string
	apiKeyID  string
	apiKeyRow locator.StoredKey
	// the edge
	edge     *edge.Edge
	edgeRing *ticket.KeyRing

	vms  []string
	byVM map[string]*cvm

	faultsOn atomic.Bool
	fmu      sync.Mutex
	frand    map[string]*rand.Rand // per-key S3 fault streams

	amu    sync.Mutex
	agenda agenda

	mu        sync.Mutex
	stats     ClusterStats
	script    []Op
	dbs       map[string]*cdb
	dbOrder   []string
	downSince map[string]time.Time
	ops       int // controller operations started (each has its own stream)

	stop      context.CancelFunc // stops clients and background loops
	bg        context.Context
	workers   sync.WaitGroup // clients, setup drivers, controld loops, controller operations
	teardowns []<-chan struct{}
}

func (w *cworld) count(f func(*ClusterStats)) {
	w.mu.Lock()
	f(&w.stats)
	w.mu.Unlock()
}

// inc is vm's running incarnation (nil while down).
func (w *cworld) inc(vm string) *cinc {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v := w.byVM[vm]; v != nil {
		return v.cur
	}
	return nil
}

// nodeContext is node's lifetime (nil while it is down): agents die and restart; controld, the edge and the
// clients live for the run.
func (w *cworld) nodeContext(node string) context.Context {
	if strings.HasPrefix(node, "vm") {
		if a := w.inc(node); a != nil && a.g.alive() {
			return a.ctx
		}
		return nil
	}
	return w.bg
}

func (w *cworld) agentHandler(vm string) http.Handler {
	if a := w.inc(vm); a != nil {
		return a.handler
	}
	return nil
}

func (w *cworld) heartbeatAge(vm string) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.downSince[vm]; ok {
		return w.clk.Now().Sub(t)
	}
	return 0
}

func (w *cworld) allKeys() []string {
	keys := make([]string, maxDBs)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s:db%d", tenant, i)
	}
	return keys
}

// at schedules fn d from now on the agenda (safe from any goroutine).
func (w *cworld) at(d time.Duration, name string, fn func()) {
	w.amu.Lock()
	defer w.amu.Unlock()
	w.agenda.seq++
	heap.Push(&w.agenda, event{at: w.clk.Now().Add(d), seq: w.agenda.seq, name: name, fn: fn})
}

// boot starts controld (its first key), the edge, every agent; then, in the background with faults off,
// creates the boot databases and sets each up; then turns faults on and starts the nemesis.
func (w *cworld) boot() error {
	w.store.UseClock(w.clk)
	w.store.SetCallFaults(w.callFaults)
	w.place.UseClock(w.clk)
	w.downSince = map[string]time.Time{}
	var cancel context.CancelFunc
	w.bg, cancel = context.WithCancel(context.Background())
	w.stop = cancel

	// controld: the issuer's key ring, the API key, the locator, the controller.
	w.signer, w.keys = &ticket.Signer{}, &tickettest.MemKeys{Seed: w.s.Seed}
	if _, _, err := ticket.Rotate(w.keys, w.signer, w.rotation(), w.clk.Now()); err != nil {
		return err
	}
	var seed [32]byte
	copy(seed[:], fmt.Sprintf("%032d", w.s.Seed))
	key, id, hash, err := locator.NewAPIKey(rand.NewChaCha8(seed))
	if err != nil {
		return err
	}
	w.apiKey, w.apiKeyID = key, id
	w.apiKeyRow = locator.StoredKey{Tenant: tenant, Principal: "sim", Perms: ticket.Read | ticket.Write, Hash: hash}
	w.reg = newRegistry(w)
	w.loc = locator.New(locStore{w}, w.signer, locator.Config{TicketTTL: w.s.ticketTTL, CacheMax: 1 << 20, AuthMax: 1 << 20, Clock: w.clk})
	w.loc.Reset()
	w.ctl = &control.Controller{Registry: w.reg, Placement: authority{w: w, from: nodeControld},
		Agents:  func(vm string) (control.Agent, error) { return lifecycleClient{w: w, vm: vm}, nil },
		Objects: noObjects{}, Policy: control.Policy{MaxDatabases: 1000, MaxHeartbeatAge: 10 * time.Second}, Clock: w.clk}

	// the edge
	w.edgeRing = ticket.NewKeyRing(nil)
	routes := edge.StaticRoutes{}
	for _, vm := range w.vms {
		routes[vm] = "http://" + vm
	}
	if w.edge, err = edge.New(edge.Config{Resolver: routes, Gate: edge.NewGate(w.edgeRing, 2*time.Second, 5*time.Minute, 1<<20),
		Limiter: ratelimit.NewSourceLimiter(1e6, 1e6, 1<<10), Transport: transport{n: w.net, from: nodeEdge}, DefaultTimeout: 2 * time.Second,
		MaxTimeout: 30 * time.Second, Grace: time.Second, BodyReadTimeout: 5 * time.Second, MaxBody: 1 << 20, Clock: w.clk}); err != nil {
		return err
	}
	w.workers.Go(func() { w.fetchKeys(w.bg, stream(w.s.Seed, "keys/edge"), nodeEdge, w.edgeRing) })

	for _, vm := range w.vms {
		v := w.byVM[vm]
		if err := os.MkdirAll(v.ownerDir, 0o755); err != nil {
			return err
		}
		a, err := w.startAgent(v)
		if err != nil {
			return err
		}
		v.cur = a
	}
	w.workers.Go(func() { w.controldLoops() })
	w.workers.Go(func() {
		for i := range w.s.DBs {
			w.createDB(fmt.Sprintf("db%d", i))
		}
		w.faultsOn.Store(true)
		w.tr.add("sim", "boot done: faults on")
		w.at(0, "nemesis", w.startNemesis)
	})
	return nil
}

func (w *cworld) rotation() ticket.RotationPolicy {
	return ticket.RotationPolicy{Rotate: max(w.s.faults.RotateEvery, time.Minute), Activate: 3 * w.s.keyFetch, Overlap: w.s.ticketTTL + 10*time.Second}
}

// noObjects: drops are not simulated.
type noObjects struct{}

func (noObjects) DeletePrefix(context.Context, string) (int, error) { return 0, nil }

// controldLoops are controld's background jobs on the clock: key rotation every 10 s, ResumeStuck every 5 s.
func (w *cworld) controldLoops() {
	r := stream(w.s.Seed, "controld")
	ctx := withRand(w.bg, r)
	if det.Sleep(ctx, w.clk, time.Duration(r.Int64N(int64(5*time.Second)))) != nil { // a phase (see startAgent)
		return
	}
	for i := 0; ; i++ {
		if det.Sleep(ctx, w.clk, 5*time.Second) != nil {
			return
		}
		if i%2 == 0 {
			created, removed, err := ticket.Rotate(w.keys, w.signer, w.rotation(), w.clk.Now())
			if created != "" || len(removed) > 0 || err != nil {
				w.tr.add("issuer", "rotated: created %q removed %v signing %s err %v", created, removed, w.signer.KID(), err != nil)
			}
		}
		rep, err := w.ctl.ResumeStuck(ctx, 0, 100, 10*time.Second)
		if err == nil && (rep.Resumed > 0 || len(rep.Failed) > 0) {
			w.tr.add("controld", "resume stuck: resumed %d failed %d", rep.Resumed, len(rep.Failed))
		}
	}
}

// settle lets every goroutine run until it blocks, then checks the store and ends the trace step.
func (w *cworld) settle() {
	synctest.Wait()
	w.model.checkGenerations()
	w.tr.flush()
}

// loop is DST-1's event loop with doors: settle; let one waiting statement through, or fire the earliest
// timer alone, or run the earliest sim event (timers first on a tie); until end.
func (w *cworld) loop(end time.Time) {
	for {
		w.settle()
		if w.model.failed() && !w.s.KeepGoing {
			return
		}
		if w.doors.release() {
			w.count(func(s *ClusterStats) { s.Steps++ })
			continue
		}
		tc, okc := w.clk.Next()
		w.amu.Lock()
		ev, oke := w.agenda.peek()
		w.amu.Unlock()
		timer := okc && (!oke || !tc.After(ev.at))
		var at time.Time
		switch {
		case timer:
			at = tc
		case oke:
			at = ev.at
		default:
			return
		}
		if at.After(end) {
			return
		}
		if d := at.Sub(time.Now()); d > 0 {
			time.Sleep(d)
			continue
		}
		w.count(func(s *ClusterStats) { s.Steps++ })
		if timer {
			w.clk.Step()
			continue
		}
		w.amu.Lock()
		heap.Pop(&w.agenda)
		w.amu.Unlock()
		w.clk.Advance(ev.at.Sub(w.clk.Now()))
		ev.fn()
	}
}

// finish stops faults, clients and loops, heals the network, kills every agent (a last restore check), and
// steps the clock until every goroutine the run started has stopped.
func (w *cworld) finish() error {
	w.faultsOn.Store(false)
	w.stop()
	w.pgDown.Store(false)
	w.store.SetFaults(durabletest.Faults{})
	done := make(chan struct{})
	go func() {
		w.workers.Wait()
		w.net.packets.Wait()
		for _, vm := range w.vms {
			if a := w.inc(vm); a != nil {
				w.mu.Lock()
				w.byVM[vm].cur = nil
				w.mu.Unlock()
				w.teardowns = append(w.teardowns, a.kill())
			}
		}
		for _, t := range w.teardowns {
			<-t
		}
		close(done)
	}()
	for range 100_000 {
		synctest.Wait()
		w.doors.release()
		w.tr.flush()
		select {
		case <-done:
			return nil
		default:
		}
		if _, ok := w.clk.Step(); !ok {
			time.Sleep(time.Second)
		}
	}
	return fmt.Errorf("sim: the run's goroutines did not stop")
}

// --- Databases --------------------------------------------------------------------------------------------------

// cdb is one database of the run.
type cdb struct {
	key   string // tenant:name
	name  string
	ready bool // set up: its clients run
}

// createDB creates name through the controller (retried until it is active), sets it up (registers and
// accounts, published), then starts its clients. Runs on its own goroutine.
func (w *cworld) createDB(name string) {
	key := tenant + ":" + name
	w.mu.Lock()
	if w.dbs[key] != nil || len(w.dbs) >= maxDBs {
		w.mu.Unlock()
		return
	}
	db := &cdb{key: key, name: name}
	w.dbs[key] = db
	w.dbOrder = append(w.dbOrder, key)
	w.mu.Unlock()
	w.model.addDB(key)
	ctx := withRand(w.bg, stream(w.s.Seed, "create/"+key))
	for try := 0; ; try++ {
		octx, cancel := det.WithTimeout(ctx, w.clk, opTimeout)
		d, err := w.ctl.Create(octx, tenant, name)
		cancel()
		w.tr.add("create/"+key, "try %d -> %s (%v)", try, d.State, err != nil)
		if err == nil && d.State == control.StateActive {
			break
		}
		if det.Sleep(ctx, w.clk, 2*time.Second) != nil {
			return
		}
	}
	if !w.setup(ctx, db) {
		return
	}
	w.mu.Lock()
	db.ready = true
	w.mu.Unlock()
	w.model.ready(key)
	for i := range w.s.Clients {
		c := newClient(w, db, i)
		w.workers.Go(func() { c.run(w.bg) })
	}
}

// setup writes the database's initial documents (one mutation, retried with its request id) and waits until a
// published generation holds them, so every later crash restores from at least the setup state.
func (w *cworld) setup(ctx context.Context, db *cdb) bool {
	c := newClient(w, db, 99)
	rid := "setup-" + db.name
	for {
		a := c.send(ctx, true, rid, setupOps(db, rid))
		if ctx.Err() != nil {
			return false
		}
		if !a.ok() {
			if det.Sleep(ctx, w.clk, time.Second) != nil {
				return false
			}
			continue
		}
		for range 120 {
			if w.model.published(db.key, rid) {
				return true
			}
			if det.Sleep(ctx, w.clk, time.Second) != nil {
				return false
			}
		}
		w.tr.add("setup/"+db.key, "setup not published after 2 min: sending it again")
	}
}

// --- S3 faults (as DST-1's callFaults) ---------------------------------------------------------------------------

func (w *cworld) callFaults(op, key string, f durabletest.Faults) durabletest.Faults {
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
	late := time.Duration(0)
	switch {
	case lose < p.LoseReply && op == durable.OpPublish && n%3 == 0:
		f.LosePublishReply, what = true, append(what, "lose-reply")
	case lose < p.LoseReply && op == durable.OpPublish && n%3 == 1:
		f.ApplyThenStale, what = true, append(what, "apply-then-412")
	case lose < p.LoseReply && op == durable.OpPublish:
		late = 50*time.Millisecond + time.Duration(n/3%uint64(10*time.Second))
		f.LandLate, what = late, append(what, "land-late "+late.String())
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
		if f.Delay > 0 {
			w.health.mark("", now, now.Add(f.Delay))
		}
		w.health.mark(key, now, now.Add(f.Delay+late))
		w.tr.add("fault/"+key, "%s %s", op, strings.Join(what, " "))
	}
	return f
}

// --- The nemesis ---------------------------------------------------------------------------------------------

// startNemesis schedules the run's operations: the given script, or each class's own exponential schedule
// (each from its own stream, so turning one class off leaves the others' schedules alone).
func (w *cworld) startNemesis() {
	if w.s.Scripted {
		for _, op := range w.s.Script {
			w.at(max(op.At-w.clk.Now().Sub(w.t0), 0), op.Kind, func() { w.apply(op) })
		}
		return
	}
	f := w.s.faults
	every := func(class string, mean time.Duration, draw func(r *rand.Rand) Op) {
		if mean <= 0 {
			return
		}
		r := stream(w.s.Seed, "nemesis/"+class)
		var next func()
		next = func() {
			if w.faultsOn.Load() {
				op := draw(r)
				op.At = w.clk.Now().Sub(w.t0)
				w.apply(op)
			}
			w.at(exp(r, mean), class, next)
		}
		w.at(exp(r, mean), class, next)
	}
	every("partition", f.PartitionEvery, func(r *rand.Rand) Op {
		nodes := w.nodesExcept("")
		node := nodes[r.IntN(len(nodes))]
		others := w.nodesExcept(node)
		var cut []string
		for _, o := range others {
			if r.IntN(2) == 0 {
				cut = append(cut, o)
			}
		}
		if len(cut) == 0 {
			cut = others
		}
		return Op{Kind: "partition", Node: node, Cut: cut, For: time.Second + uniform(r, f.PartitionMax)}
	})
	every("kill", f.KillEvery, func(r *rand.Rand) Op {
		vm := w.vms[r.IntN(len(w.vms))]
		if keys := w.readyKeys(); len(keys) > 0 && r.IntN(2) == 0 {
			if p, ok := w.place.Row(keys[r.IntN(len(keys))]); ok {
				vm = p.Owner // the owner of a database
			}
		}
		attest, d := r.Float64() < f.AttestDead, time.Second+uniform(r, f.RestartMax)
		then := "restart"
		if attest {
			then = "attest"
		}
		w.at(d, then, func() { w.apply(Op{At: w.clk.Now().Sub(w.t0), Kind: then, Node: vm}) })
		return Op{Kind: "kill", Node: vm}
	})
	every("move", f.MoveEvery, func(r *rand.Rand) Op {
		keys := w.readyKeys()
		if len(keys) == 0 {
			return Op{Kind: "move"}
		}
		return Op{Kind: "move", Node: keys[r.IntN(len(keys))]}
	})
	every("create", f.CreateEvery, func(r *rand.Rand) Op {
		w.mu.Lock()
		n := len(w.dbs)
		w.mu.Unlock()
		return Op{Kind: "create", Node: fmt.Sprintf("db%d", n)}
	})
	every("cacheloss", f.CacheLossEvery, func(*rand.Rand) Op { return Op{Kind: "cacheloss"} })
	every("storm", f.StormEvery, func(r *rand.Rand) Op { return Op{Kind: "storm", For: time.Second + uniform(r, f.StormMax)} })
	every("pgdown", f.PGDownEvery, func(r *rand.Rand) Op { return Op{Kind: "pgdown", For: time.Second + uniform(r, f.PGDownMax)} })
}

func (w *cworld) readyKeys() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, k := range w.dbOrder {
		if w.dbs[k].ready {
			out = append(out, k)
		}
	}
	return out
}

// apply runs one nemesis operation (on the event loop) and records it in the script.
func (w *cworld) apply(op Op) {
	w.mu.Lock()
	w.script = append(w.script, op)
	w.mu.Unlock()
	w.tr.add("nemesis", "%s", op)
	switch op.Kind {
	case "partition":
		heal := w.net.partition(op.Node, op.Cut)
		w.health.begin("partition", w.clk.Now())
		w.count(func(s *ClusterStats) { s.Partitions++ })
		w.at(op.For, "heal", func() {
			heal()
			w.health.end("partition", w.clk.Now())
			w.tr.add("nemesis", "healed %s", op.Node)
		})
	case "kill":
		w.kill(op.Node)
	case "restart":
		w.restart(op.Node)
	case "attest":
		w.attest(op.Node)
	case "move":
		if op.Node != "" {
			w.controllerOp("move "+op.Node, func(ctx context.Context, _ string) error {
				w.count(func(s *ClusterStats) { s.Moves++ })
				_, err := w.ctl.Move(ctx, op.Node, "")
				if err == nil {
					w.count(func(s *ClusterStats) { s.MovesDone++ })
				}
				return err
			})
		}
	case "create":
		w.count(func(s *ClusterStats) { s.Creates++ })
		w.workers.Go(func() { w.createDB(op.Node) })
	case "cacheloss":
		w.loc.Reset()
	case "storm":
		w.store.SetFaults(durabletest.Faults{Down: true})
		w.health.begin("storm", w.clk.Now())
		w.at(op.For, "storm end", func() {
			w.store.SetFaults(durabletest.Faults{})
			w.health.end("storm", w.clk.Now())
			w.tr.add("nemesis", "storm over")
		})
	case "pgdown":
		w.pgDown.Store(true)
		w.at(op.For, "pg up", func() {
			w.pgDown.Store(false)
			w.tr.add("nemesis", "pg up")
		})
	}
}

// controllerOp runs fn as one controller operation on its own goroutine, random stream, deadline and trace
// stream (several operations may finish in one step).
func (w *cworld) controllerOp(name string, fn func(ctx context.Context, trace string) error) {
	w.mu.Lock()
	w.ops++
	n := w.ops
	w.mu.Unlock()
	w.workers.Go(func() {
		ctx, cancel := det.WithTimeout(withRand(w.bg, stream(w.s.Seed, fmt.Sprintf("op/%d", n))), w.clk, opTimeout)
		defer cancel()
		trace := fmt.Sprintf("op/%d", n)
		err := fn(ctx, trace)
		w.tr.add(trace, "%s -> %s", name, lifeClass(err))
	})
}

// kill crashes vm's incarnation (kill -9). The checker learns which databases it owned (the authority's
// rows now): their histories are segmented here, and their next restore may lose writes within the RPO.
func (w *cworld) kill(vm string) {
	w.mu.Lock()
	v := w.byVM[vm]
	a := v.cur
	v.cur = nil
	if a != nil {
		w.downSince[vm] = w.clk.Now()
	}
	w.mu.Unlock()
	if a == nil {
		return
	}
	w.teardowns = append(w.teardowns, a.kill())
	w.health.end("breaker/"+vm, w.clk.Now())
	w.count(func(s *ClusterStats) { s.Kills++ })
	var owned []string
	for _, k := range w.allKeys() {
		if p, ok := w.place.Row(k); ok && p.Owner == vm {
			owned = append(owned, k)
		}
	}
	w.model.crashed(vm, owned)
	w.tr.add("sim", "kill %s (incarnation %d), owner of %v", vm, a.n, owned)
}

// restart starts vm's next incarnation, unless it is up or was attested dead.
func (w *cworld) restart(vm string) {
	w.mu.Lock()
	v := w.byVM[vm]
	skip := v.cur != nil || v.attested
	w.mu.Unlock()
	if skip {
		return
	}
	a, err := w.startAgent(v)
	if err != nil {
		w.model.violateNow("-", "restart of %s failed: %v", vm, err)
		return
	}
	w.mu.Lock()
	v.cur = a
	delete(w.downSince, vm)
	w.mu.Unlock()
	w.count(func(s *ClusterStats) { s.Restarts++ })
	w.tr.add("sim", "restart %s: incarnation %d", vm, a.n)
}

// attest has an operator attest vm dead and restore its databases elsewhere (M5), if it is down and at least
// two VMs would remain; otherwise it restarts it.
func (w *cworld) attest(vm string) {
	w.mu.Lock()
	v := w.byVM[vm]
	alive := 0
	for _, o := range w.byVM {
		if !o.attested {
			alive++
		}
	}
	ok := v.cur == nil && !v.attested && alive > 2
	if ok {
		v.attested = true
	}
	w.mu.Unlock()
	if !ok {
		w.restart(vm)
		return
	}
	w.count(func(s *ClusterStats) { s.Attests++ })
	w.controllerOp("attest+restore "+vm, func(ctx context.Context, trace string) error {
		for ctx.Err() == nil {
			err := w.ctl.AttestDead(ctx, vm, "sim")
			if err == nil {
				var rep control.RestoreReport
				if rep, err = w.ctl.Restore(ctx, vm, 10*time.Second); err == nil && len(rep.Failed) == 0 {
					w.tr.add(trace, "restored %s: %d moved", vm, len(rep.Moved))
					return nil
				}
			}
			if det.Sleep(ctx, w.clk, 2*time.Second) != nil {
				break
			}
		}
		return ctx.Err()
	})
}

// ownerChanged is an agent's owner-table transition: the checker's epoch rules, and, when the key becomes
// Active (a claim or a reopen), the restore check against the generation it starts from.
func (w *cworld) ownerChanged(a *cinc, key string, from, to owner.Local) {
	w.tr.add("owner/"+key, "%s %s -> %s", a.vm, from, to)
	w.model.ownerChanged(a.vm, key, from, to)
	if to.State == owner.Active && (from.State != owner.Active || from.Epoch != to.Epoch) {
		m, _ := w.store.Manifest(key)
		w.model.restored(key, a.vm, to.Epoch, m)
	}
}
