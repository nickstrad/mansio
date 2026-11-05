package ownertest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/owner"
)

// NewStoreFunc returns a fresh, empty LocalStore, and a reopen func that returns a new instance over the same
// backing storage (a process restart).
type NewStoreFunc func(t *testing.T) (s owner.LocalStore, reopen func() owner.LocalStore)

// RunLocalStoreSuite checks the owner.LocalStore contract. Run it against every implementation.
func RunLocalStoreSuite(t *testing.T, newStore NewStoreFunc) {
	ctx := context.Background()
	load := func(t *testing.T, s owner.LocalStore) []owner.Record {
		t.Helper()
		recs, err := s.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		slices.SortFunc(recs, func(a, b owner.Record) int { return strings.Compare(a.Key, b.Key) })
		return recs
	}
	put := func(t *testing.T, s owner.LocalStore, r owner.Record) {
		t.Helper()
		if err := s.Put(ctx, r); err != nil {
			t.Fatalf("Put(%+v): %v", r, err)
		}
	}

	t.Run("empty store loads nothing", func(t *testing.T) {
		s, _ := newStore(t)
		if got := load(t, s); len(got) != 0 {
			t.Fatalf("Load = %+v, want none", got)
		}
	})

	t.Run("put survives reopen; overwrite replaces", func(t *testing.T) {
		s, reopen := newStore(t)
		a := owner.Record{Key: "a", State: owner.Active, Epoch: 1, Gen: 0}
		put(t, s, a)
		f := owner.Record{Key: "a", State: owner.Fenced, Epoch: 1, Gen: 7, Superseded: true}
		put(t, s, f)
		put(t, s, owner.Record{Key: "b", State: owner.Active, Epoch: 3, Gen: 2})
		got := load(t, reopen())
		want := []owner.Record{f, {Key: "b", State: owner.Active, Epoch: 3, Gen: 2}}
		if !slices.Equal(got, want) {
			t.Fatalf("after reopen Load = %+v, want %+v", got, want)
		}
	})

	t.Run("delete is durable and deleting a missing key is fine", func(t *testing.T) {
		s, reopen := newStore(t)
		put(t, s, owner.Record{Key: "a", State: owner.Fenced, Epoch: 2})
		if err := s.Delete(ctx, "a"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := s.Delete(ctx, "never"); err != nil {
			t.Fatalf("Delete(missing): %v", err)
		}
		if got := load(t, reopen()); len(got) != 0 {
			t.Fatalf("after delete Load = %+v", got)
		}
	})

	// Keys at the length boundaries and with characters that are unsafe in file names.
	t.Run("any valid key round-trips", func(t *testing.T) {
		s, reopen := newStore(t)
		keys := []string{"x", strings.Repeat("k", owner.MaxKeyLen), "../etc/passwd", "a/b", "A", "a", "é ü", ".", "-x", "a\x00b"}
		for i, k := range keys {
			put(t, s, owner.Record{Key: k, State: owner.Active, Epoch: owner.Epoch(i + 1)})
		}
		got := load(t, reopen())
		if len(got) != len(keys) {
			t.Fatalf("Load returned %d records, want %d (case or path collision?): %+v", len(got), len(keys), got)
		}
		for _, r := range got {
			i := slices.Index(keys, r.Key)
			if i < 0 || r.Epoch != owner.Epoch(i+1) {
				t.Fatalf("record %+v does not match what was put", r)
			}
		}
	})

	t.Run("invalid records are rejected", func(t *testing.T) {
		s, _ := newStore(t)
		for _, r := range []owner.Record{
			{Key: "", State: owner.Active, Epoch: 1},                                     // off point: empty key
			{Key: strings.Repeat("k", owner.MaxKeyLen+1), State: owner.Active, Epoch: 1}, // off point: too long
			{Key: "a", State: owner.Active, Epoch: 0},                                    // epoch 0
			{Key: "a", State: owner.Fencing, Epoch: 1},                                   // transient state
			{Key: "a", State: owner.Closed, Epoch: 1},
		} {
			if err := s.Put(ctx, r); !errors.Is(err, owner.ErrInvalid) {
				t.Errorf("Put(%+v) = %v, want ErrInvalid", r, err)
			}
		}
		if got := load(t, s); len(got) != 0 {
			t.Fatalf("rejected puts left %+v", got)
		}
	})

	t.Run("concurrent puts to different keys all land", func(t *testing.T) {
		s, reopen := newStore(t)
		var wg sync.WaitGroup
		errc := make(chan error, 32)
		for i := range 32 {
			wg.Go(func() {
				errc <- s.Put(ctx, owner.Record{Key: string(rune('A' + i)), State: owner.Active, Epoch: 1})
			})
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			if err != nil {
				t.Fatal(err)
			}
		}
		if got := load(t, reopen()); len(got) != 32 {
			t.Fatalf("Load = %d records, want 32", len(got))
		}
	})
}
