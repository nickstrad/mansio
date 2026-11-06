package sim

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/agentglue"
	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

// cvm is one agent VM: what outlives its incarnations (its owner directory), and the running one.
type cvm struct {
	name     string
	ownerDir string
	incs     int
	cur      *cinc // nil while down
	attested bool  // an operator attested it dead: it never comes back
}

// cinc is one incarnation of agentd on a VM, wired as cmd/agentd's durable mode through internal/agentglue,
// with /v1/ops on (docexec), tickets verified against the issuer's keys it fetches, and the lifecycle calls
// (claim, fence, forget) answered by agentglue.Lifecycle.
type cinc struct {
	vm      string
	n       int
	g       *gate
	ctx     context.Context // ends at the crash
	cancel  context.CancelFunc
	host    *vactor.Host
	tab     *owner.Table
	srv     *front.Server
	handler http.Handler
	ring    *ticket.KeyRing
	life    agentglue.Lifecycle
	wg      sync.WaitGroup
}

type randKey struct{}
type seqKey struct{}

// ctxRand is the stream a goroutine's network calls draw from (a handler's: seeded by its call; an operation's:
// its own).
func ctxRand(ctx context.Context) *rand.Rand {
	r, _ := ctx.Value(randKey{}).(*rand.Rand)
	return r
}

func withRand(ctx context.Context, r *rand.Rand) context.Context {
	return context.WithValue(ctx, randKey{}, r)
}

// startAgent starts incarnation n of vm: an empty cache directory, the owner directory as the last
// incarnation left it, keys fetched from controld at once, reconcile in the background (agentd's startup).
func (w *cworld) startAgent(v *cvm) (*cinc, error) {
	v.incs++
	a := &cinc{vm: v.name, n: v.incs, g: &gate{}, ring: ticket.NewKeyRing(nil)}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	tr, name := w.tr, fmt.Sprintf("%s/%d", v.name, a.n)
	b, err := breaker.New(breaker.Config{OnChange: func(t breaker.Transition) {
		if a.g.alive() {
			tr.add("breaker/"+v.name, "%s -> %s", t.From, t.To)
			if t.To == breaker.Closed {
				w.health.end("breaker/"+v.name, w.clk.Now())
			} else {
				w.health.begin("breaker/"+v.name, w.clk.Now())
			}
		}
	}})
	if err != nil {
		return nil, err
	}
	store := durable.WithBreaker(gatedStore{g: a.g, s: s3View{w: w, vm: v.name}, tr: tr, keys: w.allKeys()}, b, nil, w.clk.Now)
	var tab *owner.Table
	a.host, err = vactor.NewHost(nil, actordb.Opener{Cfg: actor.Config{Clock: w.clk, ReadConns: w.s.readConns}}, vactor.Options{
		CacheDir: filepath.Join(w.s.Dir, v.name, fmt.Sprintf("cache-%d", a.n)), Store: store, Policy: w.s.policy, Workers: 1,
		Clock: w.clk, Rand: stream(w.s.Seed, "vactor/"+name),
		CheckActivation: agentglue.ActivationCheck(w.clk, func() *owner.Table { return tab }),
		Guard:           agentglue.PublishGuard(func() *owner.Table { return tab }, func() *vactor.Host { return a.host }),
	})
	if err != nil {
		return nil, err
	}
	fs, err := owner.NewFileStore(v.ownerDir)
	if err != nil {
		a.host.Close()
		return nil, err
	}
	tab, err = owner.Open(a.ctx, owner.Config{Self: v.name, Store: gatedLocal{a.g, fs}, Placement: gatedPlacement{a.g, authority{w: w, from: v.name}},
		WriteBack: agentglue.WriteBack{Host: a.host, Store: store},
		OnChange: func(key string, from, to owner.Local) {
			if a.g.alive() {
				w.ownerChanged(a, key, from, to)
			}
		}})
	if err != nil {
		a.host.Close()
		return nil, err
	}
	a.tab, a.life = tab, agentglue.Lifecycle{Tab: tab, Host: a.host}
	exec := commits{w: w, vm: v.name, g: a.g, Executor: agentglue.DepExecutor{Executor: agentglue.Executor{Host: a.host, Grace: time.Second,
		Order: w.doors.wait}}}
	a.srv = front.NewServer(front.Config{}, ticket.NewCache(ticket.Verifier{Keys: a.ring, VM: v.name, Skew: 2 * time.Second, MaxTTL: 5 * time.Minute}, 1<<20),
		agentglue.DepAdmitter{Admitter: agentglue.Admitter{T: tab}}, exec, w.clk)
	a.srv.EnableOps(front.OpsConfig{Schemas: simSchemas{}, IDs: docexec.NewUUIDv7(w.clk, det.NewRand(subSeed(w.s.Seed, "ids/"+name)))})
	h := front.NewHandler(a.srv, slog.New(internalLog{w: w}))
	a.handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), seqKey{}, w.seq.Add(1))))
	})
	phase := time.Microsecond + time.Duration(stream(w.s.Seed, "phase/"+name).Int64N(int64(time.Millisecond)))
	a.wg.Go(func() {
		// A random phase under 1 ms, so no two agents' ticks fall on one instant (timers due at one instant fire
		// in creation order, and goroutines started together create theirs in the scheduler's order). Small,
		// so the first write-back comes when agentd's would (a phase of up to a tick broke the RPO bound).
		if det.Sleep(a.ctx, w.clk, phase) != nil {
			return
		}
		_ = a.host.RunMaintenance(a.ctx, w.s.Tick, func(r vactor.TickReport) { w.ticked(a, r) })
	})
	a.wg.Go(func() { w.fetchKeys(a.ctx, stream(w.s.Seed, "keys/"+name), v.name, a.ring) })
	a.wg.Go(func() {
		_ = tab.ReconcileUntilDone(a.ctx, owner.ReconcileOptions{Workers: 1, Clock: w.clk, Rand: stream(w.s.Seed, "reconcile/"+name)},
			func(r owner.ReconcileReport, err error) {
				if a.g.alive() && r.Tried > 0 {
					tr.add("reconcile/"+v.name, "round %d: tried %d failed %d", r.Round, r.Tried, r.Failed)
				}
			})
	})
	return a, nil
}

// kill crashes the incarnation (as DST-1's agent.kill): cut the gate, cancel its work, tear the host down.
func (a *cinc) kill() <-chan struct{} {
	a.g.kill()
	a.cancel()
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		a.host.Close()
		a.wg.Wait()
	}()
	return ch
}

// fetchKeys is a key fetcher (agentd's keys_url, the edge's keyfetch): fetch the issuer's public keys from
// controld now, then every keyFetch ± 20 %, installing each answer.
func (w *cworld) fetchKeys(ctx context.Context, r *rand.Rand, from string, ring *ticket.KeyRing) {
	ctx = withRand(ctx, r)
	for {
		cctx, cancel := det.WithTimeout(ctx, w.clk, 2*time.Second)
		v, err := w.net.call(cctx, r, from, nodeControld, nil, nil, func(context.Context) any { return w.signer.Public() })
		cancel()
		if err == nil {
			f := v.(ticket.PublicKeyFile)
			keys, perr := ticket.ParsePublicKeys(strings.NewReader(mustJSON(f)))
			if perr == nil && !ring.Equal(keys) {
				ring.Replace(keys)
				w.tr.add("keys/"+from, "installed %v", slices.Sorted(slices.Values(ring.KIDs())))
			}
		}
		wait := w.s.keyFetch*8/10 + time.Duration(r.Int64N(int64(w.s.keyFetch*4/10)))
		if err != nil {
			wait = time.Second + time.Duration(r.Int64N(int64(time.Second)))
		}
		if det.Sleep(ctx, w.clk, wait) != nil {
			return
		}
	}
}

// ticked records a maintenance tick (as DST-1's world.ticked): what it published is durable as of the tick's
// start.
func (w *cworld) ticked(a *cinc, r vactor.TickReport) {
	if !a.g.alive() {
		return
	}
	start := w.clk.Now().Add(-r.Took)
	var wrote, passivated, failed []string
	for _, res := range r.WroteBack {
		wrote = append(wrote, fmt.Sprintf("%s:g%d", res.Key, res.Generation))
		w.model.durable(res.Key, start)
	}
	for _, ev := range r.Plan.Passivate {
		if r.Errors[ev.Key] == nil {
			passivated = append(passivated, ev.Key+":"+string(ev.Reason))
			w.model.durable(ev.Key, start)
		}
	}
	for k, err := range r.Errors {
		failed = append(failed, k+":"+errClass(err))
	}
	slices.Sort(wrote)
	slices.Sort(passivated)
	slices.Sort(failed)
	if len(wrote)+len(passivated)+len(failed) > 0 {
		w.tr.add("tick/"+a.vm, "wrote [%s] passivated [%s] failed [%s]", strings.Join(wrote, " "), strings.Join(passivated, " "), strings.Join(failed, " "))
	}
}

// commits is the agent's executor as the checker sees it: every write that commits (ok, not a duplicate) in a
// live process is recorded with its request id, epoch, VM and time, for the one-owner-per-epoch check and for
// telling a committed write lost at a crash from one that never ran. (A dead process's actor may still finish
// a write while it is torn down: that commit reaches nothing durable and its answer is never sent.)
type commits struct {
	front.Executor
	w  *cworld
	vm string
	g  *gate
}

func (c commits) Exec(ctx context.Context, call front.Call) (front.Reply, error) {
	rep, err := c.Executor.Exec(ctx, call)
	if err == nil && call.Kind == front.KindWrite && call.RequestID != "" && !rep.Duplicate && c.g.alive() {
		c.w.model.committed(c.w.clk.Now(), call.DB, c.vm, call.Epoch, call.RequestID)
	}
	return rep, err
}

// doors orders statement submissions: a request reaching its database's actor waits here until the event
// loop lets it through, lowest arrival number first, one per step. Several requests woken by one event (an
// activation finishing) would otherwise race to the actor, and their order would be the Go scheduler's.
type doors struct {
	mu      sync.Mutex
	waiting map[int64]chan struct{}
}

func (d *doors) wait(ctx context.Context, _ string) error {
	seq, _ := ctx.Value(seqKey{}).(int64)
	ch := make(chan struct{})
	d.mu.Lock()
	if d.waiting == nil {
		d.waiting = map[int64]chan struct{}{}
	}
	for d.waiting[seq] != nil { // a request without an arrival number (none today) or a reused one
		seq++
	}
	d.waiting[seq] = ch
	d.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.waiting, seq)
		d.mu.Unlock()
		select {
		case <-ch: // released meanwhile: go ahead
			return nil
		default:
		}
		return fmt.Errorf("%w: %w", front.ErrExpired, ctx.Err())
	}
}

// release lets the earliest waiter through; it reports whether there was one.
func (d *doors) release() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.waiting) == 0 {
		return false
	}
	first := int64(-1)
	for s := range d.waiting {
		if first < 0 || s < first {
			first = s
		}
	}
	close(d.waiting[first])
	delete(d.waiting, first)
	return true
}

// s3View is one VM's view of the store: cut off while a partition separates it from S3.
type s3View struct {
	w  *cworld
	vm string
}

var errPartitioned = fmt.Errorf("%w: sim: partitioned from the store", durable.ErrUnavailable)

func (s s3View) cut() bool { return s.w.net.blocked(s.vm, nodeS3) }

func (s s3View) ReadManifest(ctx context.Context, key string) (durable.Manifest, durable.Version, error) {
	if s.cut() {
		return durable.Manifest{}, "", errPartitioned
	}
	return s.w.store.ReadManifest(ctx, key)
}

func (s s3View) PutGeneration(ctx context.Context, m durable.Manifest, path string) error {
	if s.cut() {
		return errPartitioned
	}
	return s.w.store.PutGeneration(ctx, m, path)
}

func (s s3View) StatGeneration(ctx context.Context, object string) (durable.GenInfo, error) {
	if s.cut() {
		return durable.GenInfo{}, errPartitioned
	}
	return s.w.store.StatGeneration(ctx, object)
}

func (s s3View) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (durable.Fetched, error) {
	if s.cut() {
		return durable.Fetched{}, errPartitioned
	}
	return s.w.store.FetchGeneration(ctx, m, path)
}

func (s s3View) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	if s.cut() {
		return "", errPartitioned
	}
	return s.w.store.PublishManifest(ctx, m, prev)
}

func (s s3View) DeleteGenerations(ctx context.Context, key string, names []string) error {
	if s.cut() {
		return errPartitioned
	}
	return s.w.store.DeleteGenerations(ctx, key, names)
}

// internalLog collects the causes of internal answers (front's ERROR lines) for Stats.Internal.
type internalLog struct{ w *cworld }

func (l internalLog) Enabled(_ context.Context, lv slog.Level) bool { return lv >= slog.LevelError }
func (l internalLog) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			msg += ": " + a.Value.String()
		}
		return true
	})
	l.w.count(func(s *ClusterStats) { s.Internal = append(s.Internal, msg) })
	return nil
}
func (l internalLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l internalLog) WithGroup(string) slog.Handler      { return l }
