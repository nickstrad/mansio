package ownertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/mansio/server/internal/owner"
)

// SuiteVMs returns the owner names RunPlacementSuite puts in rows. An implementation that only accepts
// registered owners (the Postgres adapter) registers these in its NewPlacementFunc.
func SuiteVMs() []string { return []string{"vm-a", "vm-b", "vm-c"} }

// NewPlacementFunc returns a fresh, empty authority that accepts SuiteVMs as owners.
type NewPlacementFunc func(t *testing.T) owner.Placement

// RunPlacementSuite checks the owner.Placement contract (ports.go). Run it against every implementation:
// the fake (TestPlacementCAS here), the file-backed single-node adapter and Postgres.
func RunPlacementSuite(t *testing.T, newPlacement NewPlacementFunc) {
	ctx := context.Background()
	vmA, vmB, vmC := SuiteVMs()[0], SuiteVMs()[1], SuiteVMs()[2]
	active := func(vm string, e owner.Epoch, g owner.Generation) owner.Place {
		return owner.Place{Owner: vm, Epoch: e, Gen: g, State: owner.PlaceActive}
	}
	moving := func(vm string, e owner.Epoch, g owner.Generation) owner.Place {
		return owner.Place{Owner: vm, Epoch: e, Gen: g, State: owner.PlaceMoving}
	}
	cas := func(t *testing.T, p owner.Placement, key string, expect, next owner.Place) {
		t.Helper()
		if err := p.CAS(ctx, key, expect, next); err != nil {
			t.Fatalf("CAS(%q, %+v → %+v): %v", key, expect, next, err)
		}
	}
	wantRow := func(t *testing.T, p owner.Placement, key string, want owner.Place) {
		t.Helper()
		got, err := p.Get(ctx, key)
		if err != nil || got != want {
			t.Fatalf("Get(%q) = %+v, %v; want %+v", key, got, err, want)
		}
	}
	wantNoRow := func(t *testing.T, p owner.Placement, key string) {
		t.Helper()
		if got, err := p.Get(ctx, key); !errors.Is(err, owner.ErrNotFound) {
			t.Fatalf("Get(%q) = %+v, %v; want ErrNotFound", key, got, err)
		}
	}

	t.Run("get on a missing key is ErrNotFound", func(t *testing.T) {
		wantNoRow(t, newPlacement(t), "missing")
	})

	t.Run("create, then get returns the row", func(t *testing.T) {
		p := newPlacement(t)
		cas(t, p, "k", owner.Place{}, active(vmA, 1, 0))
		wantRow(t, p, "k", active(vmA, 1, 0))
		cas(t, p, "m", owner.Place{}, moving(vmB, 4, 9)) // any valid row may be created
		wantRow(t, p, "m", moving(vmB, 4, 9))
	})

	t.Run("create on an existing key conflicts and changes nothing", func(t *testing.T) {
		p := newPlacement(t)
		cas(t, p, "k", owner.Place{}, active(vmA, 1, 0))
		if err := p.CAS(ctx, "k", owner.Place{}, active(vmB, 2, 0)); !errors.Is(err, owner.ErrConflict) {
			t.Fatalf("second create = %v, want ErrConflict", err)
		}
		wantRow(t, p, "k", active(vmA, 1, 0))
	})

	t.Run("a non-zero expect on a missing key conflicts and creates nothing", func(t *testing.T) {
		p := newPlacement(t)
		if err := p.CAS(ctx, "k", active(vmA, 1, 0), active(vmA, 1, 1)); !errors.Is(err, owner.ErrConflict) {
			t.Fatalf("CAS on a missing key = %v, want ErrConflict", err)
		}
		wantNoRow(t, p, "k")
	})

	// Each field of expect is compared: one off point per field (and both sides of the numeric ones).
	t.Run("expect must match all four fields", func(t *testing.T) {
		base := active(vmA, 3, 5)
		next := active(vmA, 3, 6)
		p := newPlacement(t) // one key per case: keys are independent
		for i, stale := range []owner.Place{
			active(vmB, 3, 5), active(vmA, 2, 5), active(vmA, 4, 5), active(vmA, 3, 4), active(vmA, 3, 6), moving(vmA, 3, 5),
		} {
			key := fmt.Sprintf("k%d", i)
			cas(t, p, key, owner.Place{}, base)
			if err := p.CAS(ctx, key, stale, next); !errors.Is(err, owner.ErrConflict) {
				t.Fatalf("CAS from stale %+v = %v, want ErrConflict", stale, err)
			}
			wantRow(t, p, key, base)
		}
		cas(t, p, "k", owner.Place{}, base)
		cas(t, p, "k", base, next) // on point
		wantRow(t, p, "k", next)
	})

	t.Run("a move: begin, flip, and a replayed flip conflicts", func(t *testing.T) {
		p := newPlacement(t)
		cas(t, p, "k", owner.Place{}, active(vmA, 1, 0))
		cas(t, p, "k", active(vmA, 1, 0), moving(vmA, 1, 0))
		cas(t, p, "k", moving(vmA, 1, 0), active(vmB, 2, 7))
		wantRow(t, p, "k", active(vmB, 2, 7))
		// The Mover's resume after a lost reply sends the flip again: it must not apply twice.
		if err := p.CAS(ctx, "k", moving(vmA, 1, 0), active(vmB, 2, 7)); !errors.Is(err, owner.ErrConflict) {
			t.Fatalf("replayed flip = %v, want ErrConflict", err)
		}
		// And the next move flips again from the new owner.
		cas(t, p, "k", active(vmB, 2, 7), moving(vmB, 2, 7))
		cas(t, p, "k", moving(vmB, 2, 7), active(vmC, 3, 8))
		wantRow(t, p, "k", active(vmC, 3, 8))
	})

	// The authority's invariants: epochs never decrease, and the owner changes only with an epoch increase.
	// A refused change is an error that is not ErrConflict (the expected row matched) and changes nothing.
	t.Run("changes that break the invariants are refused, not conflicts", func(t *testing.T) {
		base := active(vmA, 3, 5)
		p := newPlacement(t)
		for i, bad := range []owner.Place{
			active(vmA, 2, 5), // epoch decrease (off point of "never decrease")
			active(vmB, 3, 5), // owner change at the same epoch (off point of "with an increase")
			active(vmB, 2, 5), // owner change with a decrease
			active(vmA, 0, 5), // epoch 0
		} {
			key := fmt.Sprintf("bad%d", i)
			cas(t, p, key, owner.Place{}, base)
			err := p.CAS(ctx, key, base, bad)
			if err == nil || errors.Is(err, owner.ErrConflict) || errors.Is(err, owner.ErrNotFound) {
				t.Fatalf("CAS %+v → %+v = %v; want a refusal that is neither ErrConflict nor ErrNotFound", base, bad, err)
			}
			wantRow(t, p, key, base)
		}
		if err := p.CAS(ctx, "k", owner.Place{}, active(vmA, 0, 0)); err == nil || errors.Is(err, owner.ErrConflict) {
			t.Fatalf("create at epoch 0 = %v; want a refusal that is not ErrConflict", err)
		}
		wantNoRow(t, p, "k")
		// On points: an owner change with epoch + 1, and a generation bump on the same owner and epoch.
		cas(t, p, "k", owner.Place{}, base)
		cas(t, p, "k", base, active(vmB, 4, 5))
		cas(t, p, "k", active(vmB, 4, 5), active(vmB, 4, 6))
		wantRow(t, p, "k", active(vmB, 4, 6))
	})

	t.Run("keys are exact and independent", func(t *testing.T) {
		p := newPlacement(t)
		keys := []string{"x", strings.Repeat("k", owner.MaxKeyLen), "a/b", "A", "a", "é ü", "-x", "a b"}
		for i, k := range keys {
			cas(t, p, k, owner.Place{}, active(vmA, owner.Epoch(i+1), 0))
		}
		for i, k := range keys {
			wantRow(t, p, k, active(vmA, owner.Epoch(i+1), 0))
		}
		wantNoRow(t, p, "a/")
		wantNoRow(t, p, "A/b")
	})

	// Many workers on the same key: exactly one CAS from a given row wins.
	t.Run("concurrent CAS from the same row: exactly one wins", func(t *testing.T) {
		p := newPlacement(t)
		base := active(vmA, 1, 0)
		cas(t, p, "k", owner.Place{}, base)
		const n = 32
		errc := make(chan error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { errc <- p.CAS(ctx, "k", base, active(SuiteVMs()[i%3], 2, owner.Generation(i))) })
		}
		wg.Wait()
		close(errc)
		won := 0
		for err := range errc {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, owner.ErrConflict):
				t.Fatalf("losing CAS = %v, want ErrConflict", err)
			}
		}
		got, err := p.Get(ctx, "k")
		if won != 1 || err != nil || got.Epoch != 2 {
			t.Fatalf("%d winners, row %+v %v; want exactly one winner at epoch 2", won, got, err)
		}
	})

	t.Run("concurrent creates: exactly one wins", func(t *testing.T) {
		p := newPlacement(t)
		const n = 32
		errc := make(chan error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { errc <- p.CAS(ctx, "k", owner.Place{}, active(SuiteVMs()[i%3], owner.Epoch(i+1), 0)) })
		}
		wg.Wait()
		close(errc)
		won := 0
		for err := range errc {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, owner.ErrConflict):
				t.Fatalf("losing create = %v, want ErrConflict", err)
			}
		}
		if won != 1 {
			t.Fatalf("%d creates won, want exactly 1", won)
		}
	})
}
