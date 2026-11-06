package sim

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
)

// Violation is a broken property, with the simulated time it was found at.
type Violation struct {
	At   time.Duration // since the run started
	Key  string
	What string
}

func (v Violation) String() string { return fmt.Sprintf("t=%v %s: %s", v.At, v.Key, v.What) }

// status is what the client side knows about one write (one request id).
type status uint8

const (
	pending status = iota // sent; no answer said anything yet
	none                  // every answer said nothing ran
	maybe                 // an answer left it open: ambiguous, internal, lost, or from a process that then died
	acked                 // the agent acknowledged it
	lost                  // it was applied, then a crash restored a generation without it
)

func (s status) String() string { return [...]string{"pending", "none", "maybe", "acked", "lost"}[s] }

type entry struct {
	id      string
	st      status
	ackedAt time.Time
}

// keyModel is one database's history as the clients saw it.
//
// Invariant: live lists, in commit order, the writes sent since the last restore (and the ones the restored
// generation holds). Commit order is send order because each database has one sequential client, and the
// sim's SQLite does every write in zero simulated time, so no write of a key is still queued when its client
// sends the next.
type keyModel struct {
	key  string
	live []*entry
	byID map[string]*entry
	// durableBefore: every write acknowledged before this instant is in the published generation (a write-back
	// that started then succeeded, or a restore).
	durableBefore time.Time
	gen, epoch    uint64   // the last manifest the checker saw
	genMarks      []string // what that generation holds
}

// model is the checker: the history of every database plus the store's health, against which it judges each
// answer, read, published generation and crash.
type model struct {
	mu     sync.Mutex
	t0     time.Time
	rpo    time.Duration
	health *health
	tr     *trace
	keys   map[string]*keyModel
	found  []Violation
	lostOK int // writes lost inside the RPO (allowed)
}

func newModel(t0 time.Time, rpo time.Duration, h *health, tr *trace, keys []string) *model {
	m := &model{t0: t0, rpo: rpo, health: h, tr: tr, keys: map[string]*keyModel{}}
	for _, k := range keys {
		m.keys[k] = &keyModel{key: k, byID: map[string]*entry{}, durableBefore: t0}
	}
	return m
}

func (m *model) violate(at time.Time, key, format string, args ...any) {
	v := Violation{At: at.Sub(m.t0), Key: key, What: fmt.Sprintf(format, args...)}
	m.found = append(m.found, v)
	m.tr.add("check", "VIOLATION %s: %s", key, v.What)
}

func (m *model) violations() []Violation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.found)
}

// sent records that a client sends write id. A write sent again after a crash lost it rejoins the history at
// the end (that is where it applies now).
func (m *model) sent(key, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	km := m.keys[key]
	e := km.byID[id]
	switch {
	case e == nil:
		e = &entry{id: id}
		km.byID[id] = e
	case slices.Contains(km.live, e):
		return
	case e.st == lost:
		e.st = pending
	}
	km.live = append(km.live, e)
}

// nothingRan reports whether an answer says the request did nothing (wire contract, front/codes.go).
func nothingRan(a Answer) bool {
	switch a.Code {
	case front.Shed, front.Unavailable, front.StmtTimeout, front.Aborted, front.WrongOwner, front.Fenced,
		front.TicketInvalid, front.BadRequest:
		return true
	case front.Expired:
		return !a.Ambiguous
	}
	return false
}

// answer judges one answer to r. alive is false when the incarnation died before the answer could leave it:
// the client never got it, and what it says is not known to have reached the client.
func (m *model) answer(at time.Time, key string, r Request, a Answer, alive bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	km := m.keys[key]
	if r.Kind == Read {
		if alive && a.Code == front.OK {
			m.checkMarks(at, km, a.Marks, "live read", true)
		}
		return
	}
	e := km.byID[r.ID]
	raise := func(to status) {
		if e.st == pending || e.st == none && to == maybe {
			e.st = to
		}
	}
	switch {
	case !alive:
		raise(maybe)
	case a.Code == front.OK && r.Kind == BadWrite:
		m.violate(at, key, "write %s, which the database rejects, was acknowledged", r.ID)
		e.st, e.ackedAt = acked, at
	case a.Code == front.OK && !a.Duplicate:
		if e.st == acked {
			m.violate(at, key, "write %s applied twice: a second ok without duplicate", r.ID)
		}
		e.st, e.ackedAt = acked, at
	case a.Code == front.OK: // duplicate
		switch e.st {
		case acked:
		case maybe:
			e.st, e.ackedAt = acked, at
		default:
			m.violate(at, key, "write %s answered duplicate, but it was never applied (status %s)", r.ID, e.st)
		}
	case a.Code == front.BadRequest && r.Kind == Write:
		m.violate(at, key, "valid write %s refused as bad_request", r.ID)
		raise(none)
	case nothingRan(a):
		raise(none)
	default:
		raise(maybe)
	}
}

// durable records that a write-back or passivation of key that started at from has published: every write
// acknowledged before from is in the store.
func (m *model) durable(key string, from time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if km := m.keys[key]; from.After(km.durableBefore) {
		km.durableBefore = from
	}
}

// checkMarks checks a database's marks against the history: none twice, none unknown or lost, none of a write
// every answer said did nothing, in commit order, and no acknowledged write missing (full: anywhere; else:
// before the last mark, as in a snapshot taken at some point of the history). pre: m.mu held
func (m *model) checkMarks(at time.Time, km *keyModel, marks []string, what string, full bool) (seen map[string]bool) {
	index := make(map[*entry]int, len(km.live))
	for i, e := range km.live {
		index[e] = i
	}
	seen = make(map[string]bool, len(marks))
	last := -1
	for _, mk := range marks {
		if seen[mk] {
			m.violate(at, km.key, "%s: write %s applied twice", what, mk)
			continue
		}
		seen[mk] = true
		e := km.byID[mk]
		i, ok := index[e]
		switch {
		case e == nil:
			m.violate(at, km.key, "%s: unknown mark %q", what, mk)
			continue
		case !ok:
			m.violate(at, km.key, "%s: write %s (%s) is not in the history since the last restore", what, mk, e.st)
			continue
		case e.st == none:
			m.violate(at, km.key, "%s: write %s is applied though every answer said nothing ran", what, mk)
		}
		if i < last {
			m.violate(at, km.key, "%s: write %s is out of commit order", what, mk)
		}
		last = max(last, i)
	}
	limit := last + 1
	if full {
		limit = len(km.live)
	}
	for _, e := range km.live[:limit] {
		if e.st == acked && !seen[e.id] {
			m.violate(at, km.key, "%s: acknowledged write %s is missing", what, e.id)
		}
	}
	return seen
}

// generation checks a manifest the store holds now; marks is its generation's content (read only when the
// generation changed). Generations and epochs only rise, and each generation extends the one before.
func (m *model) generation(at time.Time, key string, man durable.Manifest, marks func() ([]string, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	km := m.keys[key]
	switch {
	case man.Generation < km.gen || man.Epoch < km.epoch:
		m.violate(at, key, "manifest went from generation %d epoch %d back to generation %d epoch %d", km.gen, km.epoch, man.Generation, man.Epoch)
		return
	case man.Generation == km.gen:
		km.epoch = man.Epoch // a stamp
		return
	}
	mk, err := marks()
	if err != nil {
		m.violate(at, key, "generation %d (the manifest's) cannot be read: %v", man.Generation, err)
		return
	}
	what := fmt.Sprintf("generation %d", man.Generation)
	if len(mk) < len(km.genMarks) || !slices.Equal(mk[:len(km.genMarks)], km.genMarks) {
		m.violate(at, key, "%s does not extend generation %d (%d marks, had %d)", what, km.gen, len(mk), len(km.genMarks))
	}
	m.checkMarks(at, km, mk, what, false)
	m.tr.add("check", "%s gen %d epoch %d: %d marks", key, man.Generation, man.Epoch, len(mk))
	km.gen, km.epoch, km.genMarks = man.Generation, man.Epoch, mk
}

// restore judges a crash at `at`: marks is what the restarted process starts from (the manifest's generation
// when it starts). Every write acknowledged before the last successful write-back must be there, and a write lost
// otherwise must not have had a fault-free stretch of RPO before the crash. Then the history becomes what
// survived.
func (m *model) restore(at time.Time, key string, marks []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	km := m.keys[key]
	seen := m.checkMarks(at, km, marks, "restored generation", false)
	var live []*entry
	for _, e := range km.live {
		if seen[e.id] {
			e.st = acked // durable now
			live = append(live, e)
			continue
		}
		if e.st == acked {
			switch healthy := m.health.longestHealthy(key, e.ackedAt, at); {
			case e.ackedAt.Before(km.durableBefore):
				m.violate(at, key, "write %s acknowledged at %v, before a write-back that published (started %v), lost at the crash",
					e.id, e.ackedAt.Sub(m.t0), km.durableBefore.Sub(m.t0))
			case healthy >= m.rpo:
				m.violate(at, key, "write %s acknowledged at %v lost at the crash after a fault-free %v (RPO %v)", e.id, e.ackedAt.Sub(m.t0), healthy, m.rpo)
			default:
				m.lostOK++
				m.tr.add("check", "%s lost %s inside the RPO (acked %v)", key, e.id, e.ackedAt.Sub(m.t0))
			}
		}
		if e.st != none {
			e.st = lost
		}
	}
	km.live, km.durableBefore = live, at
}

// health records when the store was not healthy for a database's write-backs. Global: storms, an open breaker,
// and stalls (the maintenance tick writes databases back one after another, so a stall on one delays all).
// Per database: its own failed calls and lost or late replies (a point each); they do not hold other
// databases' write-backs. The RPO check counts only fault-free time.
type health struct {
	mu    sync.Mutex
	spans map[string][]span    // by database; "" = global
	open  map[string]time.Time // named global spans still going on
}

type span struct{ from, to time.Time }

func newHealth() *health { return &health{spans: map[string][]span{}, open: map[string]time.Time{}} }

// mark records an unhealthy span for key ("" = every database).
func (h *health) mark(key string, from, to time.Time) {
	h.mu.Lock()
	h.spans[key] = append(h.spans[key], span{from, to})
	h.mu.Unlock()
}

func (h *health) begin(name string, at time.Time) {
	h.mu.Lock()
	if _, ok := h.open[name]; !ok {
		h.open[name] = at
	}
	h.mu.Unlock()
}

func (h *health) end(name string, at time.Time) {
	h.mu.Lock()
	if from, ok := h.open[name]; ok {
		delete(h.open, name)
		h.spans[""] = append(h.spans[""], span{from, at})
	}
	h.mu.Unlock()
}

// longestHealthy is the longest stretch of [a, b] that no unhealthy span of key, or global one, touches.
func (h *health) longestHealthy(key string, a, b time.Time) time.Duration {
	h.mu.Lock()
	spans := slices.Concat(h.spans[""], h.spans[key])
	for _, from := range h.open {
		spans = append(spans, span{from, b})
	}
	h.mu.Unlock()
	slices.SortFunc(spans, func(x, y span) int { return x.from.Compare(y.from) })
	longest, cur := time.Duration(0), a
	for _, s := range spans {
		if s.to.Before(cur) || s.from.After(b) {
			continue
		}
		if s.from.After(cur) {
			longest = max(longest, s.from.Sub(cur))
		}
		if s.to.After(cur) {
			cur = s.to
		}
	}
	if b.After(cur) {
		longest = max(longest, b.Sub(cur))
	}
	return longest
}
