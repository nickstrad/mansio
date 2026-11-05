package vactor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// RI-06 acceptance: 500 dirty databases, every store call takes 100 ms, a 1.5 s budget. Shutdown returns
// within the budget plus a little, every database is either published or kept on disk (never lost), and the
// write-backs that made it are the ones with the oldest unpublished writes.
func TestCloseHonorsDeadline(t *testing.T) {
	const n, budget = 500, 1500 * time.Millisecond
	r := newDurable(t, nil)
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("t/k%03d", i)
		r.put(keys[i], "v-"+keys[i]) // activated one clock second apart: k000 has the oldest unpublished writes
		r.clk.Add(time.Second)
	}
	r.s.SetFaults(durabletest.Faults{Delay: 100 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	t0 := time.Now()
	rep, err := r.h.Shutdown(ctx)
	took := time.Since(t0)
	r.ended = true

	if took > budget+500*time.Millisecond {
		t.Fatalf("Shutdown took %v, budget %v", took, budget)
	}
	if !errors.Is(err, context.DeadlineExceeded) || len(rep.Kept) == 0 {
		t.Fatalf("err %v, kept %d: the budget should have run out", err, len(rep.Kept))
	}
	if rep.WrittenBack == 0 || rep.WrittenBack+len(rep.Kept) != n || rep.Busy != 0 {
		t.Fatalf("report %+v (kept %d)", rep, len(rep.Kept))
	}
	kept := map[string]bool{}
	for _, k := range rep.Kept {
		kept[k] = true
	}
	written := 0
	for i, k := range keys {
		_, serr := os.Stat(r.h.LocalPath(k))
		switch {
		case kept[k]:
			if serr != nil || r.local(k) != "v-"+k {
				t.Fatalf("%s kept but its local copy is gone or wrong: %v", k, serr)
			}
		default:
			if r.published(k) != "v-"+k || serr == nil {
				t.Fatalf("%s neither kept nor published (local copy: %v)", k, serr)
			}
			written++
			// Oldest first: with Workers in parallel, a written key is among the first WrittenBack + Workers.
			if i >= rep.WrittenBack+8 {
				t.Fatalf("%s (#%d) written back before older keys; %d written", k, i, rep.WrittenBack)
			}
		}
	}
	if written != rep.WrittenBack || !slices.IsSorted(rep.Kept) {
		t.Fatalf("written %d, report %d", written, rep.WrittenBack)
	}
	if _, err := r.h.Get(ctx, keys[0]); !errors.Is(err, vactor.ErrClosed) {
		t.Fatalf("Get after Shutdown: %v", err)
	}
}

// A clean shutdown within the budget removes the cache directory; clean databases need no budget at all.
func TestShutdownCleanAndExpiredBudget(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	if _, err := r.h.WriteBack(context.Background(), "t/a"); err != nil {
		t.Fatal(err)
	}
	r.get("t/b") // clean, never written
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // budget already spent: clean databases still close and go
	rep, err := r.h.Shutdown(ctx)
	r.ended = true
	if err != nil || rep.Dropped != 2 || rep.WrittenBack != 0 || len(rep.Kept) != 0 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if _, err := os.Stat(r.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a clean shutdown left the cache dir")
	}
}
