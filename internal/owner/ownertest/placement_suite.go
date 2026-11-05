package ownertest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/owner"
)

// RunPlacementSuite checks the owner.Placement contract (owner/ports.go) against an implementation: the fake,
// the single-node file adapter (internal/placement/file) and the Postgres adapter (internal/placement/pg).
// newPlacement returns a fresh, empty authority; it runs once per subtest.
//
// What it pins down, for every adapter alike:
//   - Get of an unknown key matches ErrNotFound; CAS with expect.Epoch == 0 creates a row.
//   - A CAS whose expect is not the current row matches ErrConflict and changes nothing (create over an existing
//     row, a stale expect).
//   - A CAS that would break the authority's invariants (epoch going down, epoch 0, a new owner without a higher
//     epoch) fails WITHOUT matching ErrConflict or ErrNotFound (a bug, never "someone else won") and changes
//     nothing.
//   - Concurrent CASes from one expected row: exactly one wins (SQL G-22, PG-04).
func RunPlacementSuite(t *testing.T, newPlacement func(t *testing.T) owner.Placement) {
	ctx := context.Background()
	a1 := owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive}
	get := func(t *testing.T, p owner.Placement, key string) owner.Place {
		t.Helper()
		got, err := p.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		return got
	}
	mustCAS := func(t *testing.T, p owner.Placement, key string, expect, next owner.Place) {
		t.Helper()
		if err := p.CAS(ctx, key, expect, next); err != nil {
			t.Fatalf("CAS(%q, %+v → %+v): %v", key, expect, next, err)
		}
	}

	t.Run("unknown key is ErrNotFound", func(t *testing.T) {
		if _, err := newPlacement(t).Get(ctx, "nope"); !errors.Is(err, owner.ErrNotFound) {
			t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
		}
	})

	t.Run("create, then create again conflicts", func(t *testing.T) {
		p := newPlacement(t)
		mustCAS(t, p, "k", owner.Place{}, a1)
		if got := get(t, p, "k"); got != a1 {
			t.Fatalf("Get = %+v, want %+v", got, a1)
		}
		other := owner.Place{Owner: "b", Epoch: 1, State: owner.PlaceActive}
		if err := p.CAS(ctx, "k", owner.Place{}, other); !errors.Is(err, owner.ErrConflict) {
			t.Fatalf("second create = %v, want ErrConflict", err)
		}
		if got := get(t, p, "k"); got != a1 {
			t.Fatalf("a refused create changed the row: %+v", got)
		}
	})

	t.Run("stale expect conflicts and changes nothing", func(t *testing.T) {
		p := newPlacement(t)
		mustCAS(t, p, "k", owner.Place{}, a1)
		stale := a1
		stale.Gen = 3 // off point: one field differs
		next := owner.Place{Owner: "a", Epoch: 2, State: owner.PlaceActive}
		if err := p.CAS(ctx, "k", stale, next); !errors.Is(err, owner.ErrConflict) {
			t.Fatalf("CAS with a stale expect = %v, want ErrConflict", err)
		}
		if err := p.CAS(ctx, "missing", a1, next); !errors.Is(err, owner.ErrConflict) {
			t.Fatalf("CAS of a missing row with a non-zero expect = %v, want ErrConflict", err)
		}
		if got := get(t, p, "k"); got != a1 {
			t.Fatalf("row changed: %+v", got)
		}
	})

	t.Run("invariant violations are errors, not conflicts", func(t *testing.T) {
		p := newPlacement(t)
		a5 := owner.Place{Owner: "a", Epoch: 5, Gen: 2, State: owner.PlaceActive}
		mustCAS(t, p, "k", owner.Place{}, a5)
		for _, tc := range []struct {
			name string
			next owner.Place
		}{
			{"epoch goes down", owner.Place{Owner: "a", Epoch: 4, Gen: 2, State: owner.PlaceActive}},
			{"new owner at the same epoch", owner.Place{Owner: "b", Epoch: 5, Gen: 2, State: owner.PlaceActive}},
			{"epoch 0", owner.Place{Owner: "a", Epoch: 0, Gen: 2, State: owner.PlaceActive}},
		} {
			err := p.CAS(ctx, "k", a5, tc.next)
			if err == nil || errors.Is(err, owner.ErrConflict) || errors.Is(err, owner.ErrNotFound) {
				t.Errorf("%s: CAS = %v, want a non-sentinel error", tc.name, err)
			}
		}
		if got := get(t, p, "k"); got != a5 {
			t.Fatalf("a refused CAS changed the row: %+v", got)
		}
	})

	t.Run("a move: begin, flip to a new owner at epoch+1, same owner bumps", func(t *testing.T) {
		p := newPlacement(t)
		mustCAS(t, p, "k", owner.Place{}, a1)
		moving := owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceMoving}
		mustCAS(t, p, "k", a1, moving)
		b2 := owner.Place{Owner: "b", Epoch: 2, Gen: 7, State: owner.PlaceActive} // on point: epoch + 1
		mustCAS(t, p, "k", moving, b2)
		b3 := owner.Place{Owner: "b", Epoch: 3, Gen: 7, State: owner.PlaceActive}
		mustCAS(t, p, "k", b2, b3)
		if got := get(t, p, "k"); got != b3 {
			t.Fatalf("Get = %+v, want %+v", got, b3)
		}
	})

	t.Run("concurrent CAS from one row: exactly one wins", func(t *testing.T) {
		p := newPlacement(t)
		mustCAS(t, p, "k", owner.Place{}, a1)
		const n = 32
		errc := make(chan error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				next := owner.Place{Owner: "w" + string(rune('A'+i)), Epoch: 2, State: owner.PlaceActive}
				errc <- p.CAS(ctx, "k", a1, next)
			})
		}
		wg.Wait()
		close(errc)
		wins := 0
		for err := range errc {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, owner.ErrConflict):
				t.Errorf("loser got %v, want ErrConflict", err)
			}
		}
		if got := get(t, p, "k"); wins != 1 || got.Epoch != 2 || !strings.HasPrefix(got.Owner, "w") {
			t.Fatalf("%d winners, row %+v; want exactly one at epoch 2", wins, got)
		}
	})

	t.Run("keys are independent and any valid key round-trips", func(t *testing.T) {
		p := newPlacement(t)
		keys := []string{"x", strings.Repeat("k", owner.MaxKeyLen), "../etc/passwd", "a/b", "A", "a", "é ü"}
		for i, k := range keys {
			mustCAS(t, p, k, owner.Place{}, owner.Place{Owner: "a", Epoch: owner.Epoch(i + 1), State: owner.PlaceActive})
		}
		for i, k := range keys {
			if got := get(t, p, k); got.Epoch != owner.Epoch(i+1) {
				t.Fatalf("key %q: %+v (collision?)", k, got)
			}
		}
	})
}
