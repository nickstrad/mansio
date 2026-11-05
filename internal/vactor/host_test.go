package vactor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/vactortest"
)

var ctx = context.Background()

type rig struct {
	h *vactor.Host
	f *vactortest.Fetcher
	o *vactortest.Opener
}

func newRig(t *testing.T, keep bool, keys ...string) rig {
	t.Helper()
	f, o := vactortest.NewFetcher(), &vactortest.Opener{}
	for _, k := range keys {
		f.Put(k, []byte("db:"+k))
	}
	h, err := vactor.NewHost(f, o, vactor.Options{
		CacheDir:    filepath.Join(t.TempDir(), "cache"),
		WarmQueries: []string{"warm"},
		KeepLocal:   keep,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return rig{h, f, o}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestNewHostRequiresCacheDir(t *testing.T) {
	if _, err := vactor.NewHost(vactortest.NewFetcher(), &vactortest.Opener{}, vactor.Options{}); err == nil {
		t.Fatal("NewHost with no CacheDir succeeded")
	}
}

func TestNewHostEmptiesCacheDirAndCloseRemovesIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	os.MkdirAll(dir, 0o755)
	stale := filepath.Join(dir, "stale.sqlite")
	os.WriteFile(stale, []byte("x"), 0o644)

	h, err := vactor.NewHost(vactortest.NewFetcher(), &vactortest.Opener{}, vactor.Options{CacheDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if exists(stale) {
		t.Fatal("NewHost kept a stale file in the cache dir")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(dir) {
		t.Fatal("Close left the cache dir")
	}
}

func TestFirstGetActivatesLaterGetsShare(t *testing.T) {
	r := newRig(t, false, "a")

	first, err := r.h.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if first.Act == nil || first.Waited || first.Act.Bytes != int64(len("db:a")) || first.Act.LocalHit {
		t.Fatalf("first Get = %+v (act %+v), want an owner activation of the fetched bytes", first, first.Act)
	}
	if first.Act.Total < first.Act.Fetch {
		t.Fatalf("Total %v < Fetch %v", first.Act.Total, first.Act.Fetch)
	}

	again, err := r.h.Get(ctx, "a")
	if err != nil || again.Act != nil || again.Waited || again.DB != first.DB {
		t.Fatalf("second Get = %+v, %v; want the same DB, no activation, no wait", again, err)
	}
	if got := r.f.Calls("a"); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
	if s := r.h.Stats(); s != (vactor.Stats{Activations: 1, Fetches: 1}) {
		t.Fatalf("Stats = %+v", s)
	}
}

func TestConcurrentGetsOnColdKeyShareOneActivation(t *testing.T) {
	r := newRig(t, false, "a")
	r.f.Delay = 30 * time.Millisecond

	const callers = 100
	var owners, waiters atomic.Int64
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			res, err := r.h.Get(ctx, "a")
			if err != nil {
				t.Error(err)
				return
			}
			if res.Act != nil {
				owners.Add(1)
			}
			if res.Waited {
				waiters.Add(1)
			}
		})
	}
	wg.Wait()
	if r.f.Calls("a") != 1 || owners.Load() != 1 || waiters.Load() != callers-1 {
		t.Fatalf("fetches=%d owners=%d waiters=%d; want 1, 1, %d", r.f.Calls("a"), owners.Load(), waiters.Load(), callers-1)
	}
}

func TestFailedActivationIsNotCachedAndIsRetried(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		breakIt func(r rig)
		fixIt   func(r rig)
		wantErr error
	}{
		{"fetch down", "a", func(r rig) { r.f.SetDown(true) }, func(r rig) { r.f.SetDown(false) }, vactortest.ErrInjected},
		{"object missing", "missing", func(rig) {}, func(r rig) { r.f.Put("missing", []byte("x")) }, vactor.ErrNotFound},
		{"open fails", "a", func(r rig) { r.o.Set(true, false) }, func(r rig) { r.o.Set(false, false) }, vactortest.ErrInjected},
		{"query fails", "a", func(r rig) { r.o.Set(false, true) }, func(r rig) { r.o.Set(false, false) }, vactortest.ErrInjected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, false, "a")
			tc.breakIt(r)
			if _, err := r.h.Get(ctx, tc.key); !errors.Is(err, tc.wantErr) {
				t.Fatalf("broken Get err = %v, want %v", err, tc.wantErr)
			}
			if r.h.Active() != 0 || exists(r.h.LocalPath(tc.key)) {
				t.Fatalf("failed activation left state: active=%d, local file=%v", r.h.Active(), exists(r.h.LocalPath(tc.key)))
			}
			if opened, closed := r.o.Counts(); opened != closed {
				t.Fatalf("failed activation leaked an open db: opened %d closed %d", opened, closed)
			}
			tc.fixIt(r)
			if _, err := r.h.Get(ctx, tc.key); err != nil {
				t.Fatalf("Get after fix: %v", err)
			}
			if s := r.h.Stats(); s.Activations != 2 || s.Failures != 1 {
				t.Fatalf("Stats = %+v, want 2 activations, 1 failure", s)
			}
		})
	}
}

func TestPassivate(t *testing.T) {
	for _, keep := range []bool{false, true} {
		name := map[bool]string{false: "deletes local copy", true: "keeps local copy for a local hit"}[keep]
		t.Run(name, func(t *testing.T) {
			r := newRig(t, keep, "a")
			if _, err := r.h.Get(ctx, "a"); err != nil {
				t.Fatal(err)
			}
			path := r.h.LocalPath("a")
			os.WriteFile(path+"-wal", nil, 0o644) // side files SQLite would leave

			r.h.Passivate("a")
			if r.h.Active() != 0 {
				t.Fatalf("Active = %d after Passivate", r.h.Active())
			}
			if exists(path) != keep || exists(path+"-wal") != keep {
				t.Fatalf("keep=%v: db exists=%v, -wal exists=%v", keep, exists(path), exists(path+"-wal"))
			}
			if opened, closed := r.o.Counts(); opened != 1 || closed != 1 {
				t.Fatalf("opened %d closed %d, want 1 and 1", opened, closed)
			}

			res, err := r.h.Get(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if res.Act.LocalHit != keep || r.f.Calls("a") != map[bool]int{false: 2, true: 1}[keep] {
				t.Fatalf("keep=%v: LocalHit=%v fetches=%d", keep, res.Act.LocalHit, r.f.Calls("a"))
			}
		})
	}
}

func TestPassivateInactiveKeyIsNoop(t *testing.T) {
	r := newRig(t, false)
	r.h.Passivate("never-active")
	if r.h.Active() != 0 {
		t.Fatal("Passivate created an entry")
	}
}

func TestWaiterCancelDoesNotFailOwner(t *testing.T) {
	r := newRig(t, false, "a")
	r.f.Delay = 50 * time.Millisecond

	owner := make(chan error, 1)
	go func() { _, err := r.h.Get(ctx, "a"); owner <- err }()
	time.Sleep(5 * time.Millisecond) // let the owner start the activation

	wctx, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	if _, err := r.h.Get(wctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err = %v, want deadline exceeded", err)
	}
	if err := <-owner; err != nil {
		t.Fatalf("owner failed after a waiter gave up: %v", err)
	}
}

func TestGetAfterCloseFails(t *testing.T) {
	r := newRig(t, false, "a")
	r.h.Close()
	if _, err := r.h.Get(ctx, "a"); !errors.Is(err, vactor.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestCloseCancelsActivationInFlight(t *testing.T) {
	r := newRig(t, false, "a")
	r.f.Delay = time.Hour
	errc := make(chan error, 1)
	go func() { _, err := r.h.Get(ctx, "a"); errc <- err }()
	time.Sleep(5 * time.Millisecond)

	done := make(chan struct{})
	go func() { r.h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a fetch in flight")
	}
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight Get err = %v, want context.Canceled", err)
	}
}

// Regression: a Get that races a Passivate of the same key must never get a database whose local file is
// being deleted (the fake db's Query fails if its file vanished).
func TestGetRacingPassivateNeverServesDeletedFile(t *testing.T) {
	r := newRig(t, false, "a")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				res, err := r.h.Get(ctx, "a")
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if err := res.DB.Query(ctx, "check"); err != nil && !errors.Is(err, context.Canceled) &&
					err.Error() != "vactortest: query on closed db" {
					t.Errorf("served a db whose file is gone: %v", err)
					return
				}
			}
		})
		wg.Go(func() {
			for range 200 {
				r.h.Passivate("a")
			}
		})
	}
	wg.Wait()
}

type panicOpener struct{ vactortest.Opener }

func (p *panicOpener) Open(string) (vactor.DB, error) { panic("opener bug") }

// A panic inside the activation goroutine must reach the caller (so the top level can report a bug) and must
// not leave a cached entry behind.
func TestActivationPanicReachesEveryCallerAndIsNotCached(t *testing.T) {
	f := vactortest.NewFetcher()
	f.Put("a", []byte("x"))
	h, err := vactor.NewHost(f, &panicOpener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c")})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for i := range 2 {
		func() {
			defer func() {
				if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "opener bug") {
					t.Fatalf("call %d: recovered %v, want the opener's panic", i, p)
				}
			}()
			h.Get(ctx, "a")
		}()
		if h.Active() != 0 {
			t.Fatalf("call %d: panicked activation stayed cached", i)
		}
	}
}

// Every error leaving the package is a well-formed vactor error that still matches its sentinel.
func TestErrorsAreWellFormed(t *testing.T) {
	r := newRig(t, false)
	_, err := r.h.Get(ctx, "missing")
	e, ok := errs.Outermost(err)
	if !ok || e.Module != "vactor" || !errors.Is(err, vactor.ErrNotFound) {
		t.Fatalf("err = %v (%T); want a vactor error wrapping ErrNotFound", err, err)
	}
	r.h.Close()
	_, err = r.h.Get(ctx, "a")
	if e, ok := errs.Outermost(err); !ok || e.Module != "vactor" || !errors.Is(err, vactor.ErrClosed) {
		t.Fatalf("after Close err = %v; want a vactor error wrapping ErrClosed", err)
	}
}
