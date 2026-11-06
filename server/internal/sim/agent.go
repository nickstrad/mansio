package sim

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/agentglue"
	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

const (
	self  = "vm1" // the agent's owner id
	epoch = 1     // single node: the placement epoch never moves
)

// agent is one incarnation of the agent process, wired as cmd/agentd's durable mode: front pipeline →
// owner admitter → vactor host executor → actors; owner table over the owner directory and the placement
// authority; the host's store behind its own (in-memory, so per-process) breaker.
type agent struct {
	n      int
	g      *gate
	ctx    context.Context // cancelled at the crash: requests, maintenance, reconcile
	cancel context.CancelFunc
	host   *vactor.Host
	tab    *owner.Table
	srv    *front.Server
	wg     sync.WaitGroup // the agent's background loops
}

var _ Agent = (*agent)(nil)

func (a *agent) Front() *front.Server { return a.srv }

// tickets is the sim's ticket verifier: every ticket is valid for the database it names at the one epoch.
type tickets struct{}

func (tickets) Verify(_ string, now time.Time, want ticket.Want) (ticket.Claims, error) {
	return ticket.Claims{TID: "t", DBID: want.DB, VM: self, Epoch: epoch, Sub: "sim", Perms: ticket.Read | ticket.Write,
		IAT: now.Unix(), EXP: now.Add(time.Hour).Unix()}, nil
}

// newAgent starts incarnation n. With claim it claims every key (first boot); otherwise it loads the owner
// directory and reconciles in the background, as agentd's startup does.
//
//	pre: no other incarnation is alive
func (w *world) newAgent(n int, claim bool) (*agent, error) {
	a := &agent{n: n, g: &gate{}}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	tr := w.tr
	b, err := breaker.New(breaker.Config{OnChange: func(t breaker.Transition) {
		now := w.clk.Now()
		if a.g.alive() {
			tr.add("breaker", "%s -> %s", t.From, t.To)
			if t.To == breaker.Closed {
				w.health.end("breaker", now)
			} else {
				w.health.begin("breaker", now)
			}
		}
	}})
	if err != nil {
		return nil, err
	}
	store := durable.WithBreaker(gatedStore{g: a.g, s: w.store, tr: tr, keys: w.keys, applied: w.reopened,
		sent: &sentLog{m: map[string][]durable.Manifest{}}}, b, nil, w.clk.Now)
	var tab *owner.Table
	a.host, err = vactor.NewHost(nil, actordb.Opener{Cfg: actor.Config{Clock: w.clk, ReadConns: w.s.readConns}}, vactor.Options{
		CacheDir: filepath.Join(w.s.Dir, fmt.Sprintf("cache-%d", n)), Store: store, GenCacheBytes: w.s.genCache,
		Policy: w.s.policy, Workers: 1, Clock: w.clk, Rand: stream(w.s.Seed, fmt.Sprintf("vactor-%d", n)),
		CheckActivation: agentglue.ActivationCheck(w.clk, func() *owner.Table { return tab }),
		Guard:           agentglue.PublishGuard(func() *owner.Table { return tab }, func() *vactor.Host { return a.host }),
	})
	if err != nil {
		return nil, err
	}
	fs, err := owner.NewFileStore(w.ownerDir)
	if err != nil {
		a.host.Close()
		return nil, err
	}
	tab, err = owner.Open(a.ctx, owner.Config{Self: self, Store: gatedLocal{a.g, fs}, Placement: gatedPlacement{a.g, w.place},
		WriteBack: agentglue.WriteBack{Host: a.host, Store: store},
		OnChange: func(key string, from, to owner.Local) {
			if a.g.alive() {
				tr.add("owner/"+key, "%s -> %s", from, to)
			}
		}})
	if err != nil {
		a.host.Close()
		return nil, err
	}
	a.tab = tab
	a.srv = front.NewServer(front.Config{}, tickets{}, agentglue.DepAdmitter{Admitter: agentglue.Admitter{T: tab}}, agentglue.DepExecutor{Executor: agentglue.Executor{Host: a.host, Grace: time.Second}}, w.clk)
	a.wg.Go(func() {
		_ = a.host.RunMaintenance(a.ctx, w.s.Tick, func(r vactor.TickReport) { w.ticked(a, r) })
	})
	if claim {
		for _, k := range w.keys {
			if _, err := tab.Claim(a.ctx, k); err != nil {
				return nil, fmt.Errorf("sim: first-boot claim of %s: %w", k, err)
			}
		}
		return a, nil
	}
	a.wg.Go(func() {
		_ = tab.ReconcileUntilDone(a.ctx, owner.ReconcileOptions{Workers: 1, Clock: w.clk, Rand: stream(w.s.Seed, fmt.Sprintf("reconcile-%d", n))},
			func(r owner.ReconcileReport, err error) {
				if a.g.alive() {
					tr.add("reconcile", "round %d: tried %d failed %d", r.Round, r.Tried, r.Failed)
				}
			})
	})
	return a, nil
}

// ticked records a maintenance tick: what it published is durable as of the tick's start.
func (w *world) ticked(a *agent, r vactor.TickReport) {
	if !a.g.alive() {
		return
	}
	start := w.clk.Now().Add(-r.Took)
	var wrote, passivated, failed []string
	for _, res := range r.WroteBack {
		wrote = append(wrote, fmt.Sprintf("%s:g%d", res.Key, res.Generation))
		w.durable(res.Key, start)
	}
	for _, ev := range r.Plan.Passivate {
		if r.Errors[ev.Key] == nil {
			passivated = append(passivated, ev.Key+":"+string(ev.Reason))
			w.durable(ev.Key, start)
		}
	}
	for k, err := range r.Errors {
		failed = append(failed, k+":"+errClass(err))
	}
	slices.Sort(wrote)
	slices.Sort(passivated)
	slices.Sort(failed)
	if len(wrote)+len(passivated)+len(failed) > 0 {
		w.tr.add("tick", "wrote [%s] passivated [%s] failed [%s]", strings.Join(wrote, " "), strings.Join(passivated, " "), strings.Join(failed, " "))
	}
}

// kill crashes the incarnation: cut its gate, cancel its work, and tear its host down in the background (the
// teardown reaches nothing durable). done is closed when the teardown has finished.
func (a *agent) kill() (done <-chan struct{}) {
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
