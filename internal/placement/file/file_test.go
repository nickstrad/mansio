//go:build darwin || linux

package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/owner/ownertest"
)

var ctx = context.Background()

func open(t *testing.T, dir string) *Placement {
	t.Helper()
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The shared owner.Placement contract (CP-1 gate: the file adapter passes TestPlacementCAS).
func TestPlacementCAS(t *testing.T) {
	ownertest.RunPlacementSuite(t, func(t *testing.T) owner.Placement { return open(t, t.TempDir()) })
}

// Two instances over one directory are one authority (two processes on a host): concurrent CASes from the
// same row still have exactly one winner, and each sees the other's writes.
func TestTwoInstancesShareOneAuthority(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir), open(t, dir)
	a1 := owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive}
	if err := a.CAS(ctx, "k", owner.Place{}, a1); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Get(ctx, "k"); err != nil || got != a1 {
		t.Fatalf("b.Get = %+v, %v", got, err)
	}
	var wg sync.WaitGroup
	errc := make(chan error, 40)
	for i := range 40 {
		p := a
		if i%2 == 1 {
			p = b
		}
		wg.Go(func() {
			errc <- p.CAS(ctx, "k", a1, owner.Place{Owner: "n" + string(rune('A'+i)), Epoch: 2, State: owner.PlaceActive})
		})
	}
	wg.Wait()
	close(errc)
	wins := 0
	for err := range errc {
		if err == nil {
			wins++
		} else if !errors.Is(err, owner.ErrConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d winners", wins)
	}
}

func TestRowsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	m := owner.Place{Owner: "vm-1", Epoch: 4, Gen: 9, State: owner.PlaceMoving}
	if err := open(t, dir).CAS(ctx, "db/x", owner.Place{}, m); err != nil {
		t.Fatal(err)
	}
	if got, err := open(t, dir).Get(ctx, "db/x"); err != nil || got != m {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
}

// A damaged row fails closed: an error that is neither ErrNotFound (which would read as "dropped") nor
// ErrConflict, and a CAS over it changes nothing.
func TestDamagedRowFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p := open(t, dir)
	a1 := owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive}
	if err := p.CAS(ctx, "k", owner.Place{}, a1); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"not json":      "{",
		"unknown field": `{"v":1,"key":"k","owner":"a","epoch":1,"gen":0,"state":"ACTIVE","x":1}`,
		"other key":     `{"v":1,"key":"j","owner":"a","epoch":1,"gen":0,"state":"ACTIVE"}`,
		"bad state":     `{"v":1,"key":"k","owner":"a","epoch":1,"gen":0,"state":"GONE"}`,
		"epoch 0":       `{"v":1,"key":"k","owner":"a","epoch":0,"gen":0,"state":"ACTIVE"}`,
		"format 2":      `{"v":2,"key":"k","owner":"a","epoch":1,"gen":0,"state":"ACTIVE"}`,
	} {
		os.WriteFile(p.path("k"), []byte(body), 0o644)
		_, err := p.Get(ctx, "k")
		if err == nil || errors.Is(err, owner.ErrNotFound) || errors.Is(err, owner.ErrConflict) {
			t.Errorf("%s: Get = %v", name, err)
		}
		err = p.CAS(ctx, "k", a1, owner.Place{Owner: "a", Epoch: 2, State: owner.PlaceActive})
		if err == nil || errors.Is(err, owner.ErrConflict) {
			t.Errorf("%s: CAS = %v", name, err)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	p := open(t, t.TempDir())
	if _, err := p.Get(ctx, ""); !errors.Is(err, owner.ErrInvalid) {
		t.Fatalf("Get(\"\") = %v", err)
	}
	if err := p.CAS(ctx, "", owner.Place{}, owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive}); !errors.Is(err, owner.ErrInvalid) {
		t.Fatalf("CAS(\"\") = %v", err)
	}
	for _, next := range []owner.Place{{Epoch: 1, State: owner.PlaceActive}, {Owner: "a", Epoch: 1}} {
		if err := p.CAS(ctx, "k", owner.Place{}, next); err == nil || errors.Is(err, owner.ErrConflict) {
			t.Fatalf("CAS to %+v = %v", next, err)
		}
	}
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Get(cancelled, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get(cancelled) = %v", err)
	}
}

// A CAS waits for the lock only until its ctx ends (Release It T6), and changes nothing then.
func TestLockWaitHonorsDeadline(t *testing.T) {
	dir := t.TempDir()
	p := open(t, dir)
	unlock, err := p.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	dctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	err = open(t, dir).CAS(dctx, "k", owner.Place{}, owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > time.Second {
		t.Fatalf("CAS under a held lock = %v after %v", err, time.Since(t0))
	}
	if _, err := p.Get(ctx, "k"); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("a timed-out CAS wrote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
		t.Fatal(err)
	}
}
