package sim

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// errDead is what every outside call of a crashed incarnation gets.
var errDead = errors.New("sim: the process has crashed")

// gate is one incarnation's link to everything that outlives it: the store, the placement authority and the
// owner directory. kill cuts it, which is what makes a crash a crash: the dead process's goroutines may still
// run while they are torn down, but nothing they do reaches durable state. Kill happens at a quiescent point,
// so no call is half-way through the store when the gate closes; a call stalled inside the store returns
// through its cancelled context without applying (durabletest checks ctx after a stall).
type gate struct{ dead atomic.Bool }

func (g *gate) kill()       { g.dead.Store(true) }
func (g *gate) alive() bool { return !g.dead.Load() }

// gatedStore is the incarnation's view of the durable store. It traces every call that reaches the store.
type gatedStore struct {
	g    *gate
	s    durable.Store
	tr   *trace
	keys []string // to name the database a generation object belongs to
	// applied, if set, is asked after each manifest read or publish while the process lives, with the
	// manifests this process sent for the key; true forgets them.
	applied func(key string, sent []durable.Manifest) bool
	sent    *sentLog
}

// sentLog is what an incarnation published, per key, until applied says it is done with it.
type sentLog struct {
	mu sync.Mutex
	m  map[string][]durable.Manifest
}

// check asks applied about key. pre: the process is alive.
func (s gatedStore) check(key string) {
	if s.applied == nil {
		return
	}
	s.sent.mu.Lock()
	sent := slices.Clone(s.sent.m[key])
	s.sent.mu.Unlock()
	if len(sent) > 0 && s.applied(key, sent) {
		s.sent.mu.Lock()
		delete(s.sent.m, key)
		s.sent.mu.Unlock()
	}
}

var _ durable.Store = gatedStore{}

func (s gatedStore) do(key, op string, fn func() error) error {
	if !s.g.alive() {
		return errDead
	}
	err := fn()
	if s.g.alive() {
		s.tr.add("s3/"+key, "%s -> %s", op, errClass(err))
	}
	return err
}

func (s gatedStore) ReadManifest(ctx context.Context, key string) (m durable.Manifest, v durable.Version, err error) {
	err = s.do(key, "manifest.read", func() error { m, v, err = s.s.ReadManifest(ctx, key); return err })
	if s.g.alive() {
		s.check(key)
	}
	return m, v, err
}

func (s gatedStore) PutGeneration(ctx context.Context, m durable.Manifest, path string) error {
	return s.do(m.Key, "gen.put", func() error { return s.s.PutGeneration(ctx, m, path) })
}

func (s gatedStore) StatGeneration(ctx context.Context, object string) (info durable.GenInfo, err error) {
	key := "-"
	for _, k := range s.keys {
		if strings.HasPrefix(object, durable.GenPrefix(k)) {
			key = k
		}
	}
	err = s.do(key, "gen.stat", func() error { info, err = s.s.StatGeneration(ctx, object); return err })
	return info, err
}

func (s gatedStore) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (f durable.Fetched, err error) {
	err = s.do(m.Key, "gen.fetch", func() error { f, err = s.s.FetchGeneration(ctx, m, path); return err })
	return f, err
}

func (s gatedStore) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (v durable.Version, err error) {
	if s.applied != nil {
		s.sent.mu.Lock()
		s.sent.m[m.Key] = append(s.sent.m[m.Key], m)
		s.sent.mu.Unlock()
	}
	err = s.do(m.Key, "manifest.publish", func() error { v, err = s.s.PublishManifest(ctx, m, prev); return err })
	if s.g.alive() {
		s.check(m.Key)
	}
	return v, err
}

func (s gatedStore) DeleteGenerations(ctx context.Context, key string, names []string) error {
	return s.do(key, "gen.delete", func() error { return s.s.DeleteGenerations(ctx, key, names) })
}

// gatedLocal is the incarnation's owner directory (owner.FileStore behind the gate).
type gatedLocal struct {
	g *gate
	s owner.LocalStore
}

func (l gatedLocal) Put(ctx context.Context, r owner.Record) error {
	if !l.g.alive() {
		return errDead
	}
	return l.s.Put(ctx, r)
}

func (l gatedLocal) Delete(ctx context.Context, key string) error {
	if !l.g.alive() {
		return errDead
	}
	return l.s.Delete(ctx, key)
}

func (l gatedLocal) Load(ctx context.Context) ([]owner.Record, error) {
	if !l.g.alive() {
		return nil, errDead
	}
	return l.s.Load(ctx)
}

// gatedPlacement is the incarnation's link to the placement authority.
type gatedPlacement struct {
	g *gate
	p owner.Placement
}

func (p gatedPlacement) Get(ctx context.Context, key string) (owner.Place, error) {
	if !p.g.alive() {
		return owner.Place{}, errDead
	}
	return p.p.Get(ctx, key)
}

func (p gatedPlacement) CAS(ctx context.Context, key string, expect, next owner.Place) error {
	if !p.g.alive() {
		return errDead
	}
	return p.p.CAS(ctx, key, expect, next)
}

// errClass names an error by its sentinel only, never by its text: texts carry store versions and temp names
// that depend on how other databases' calls interleaved.
func errClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errDead):
		return "dead"
	case errors.Is(err, durabletest.ErrInjected):
		return "injected"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, durable.ErrStale):
		return "stale"
	case errors.Is(err, durable.ErrExists):
		return "exists"
	case errors.Is(err, durable.ErrNoManifest):
		return "no-manifest"
	case errors.Is(err, durable.ErrNotFound):
		return "not-found"
	case errors.Is(err, durable.ErrCorrupt):
		return "corrupt"
	case errors.Is(err, durable.ErrUnavailable):
		return "unavailable"
	}
	return "error"
}
