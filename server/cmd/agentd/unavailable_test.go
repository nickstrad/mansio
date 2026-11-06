package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/agentglue"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

// A write-back that sent nothing because too many publishes are in doubt (vactor.ErrInDoubt) is 503
// unavailable on the request path and on the control/admin endpoints (fence, release, move, backup), never
// internal, with a Retry-After from the grace window.
func TestPublishesInDoubtAreUnavailable(t *testing.T) {
	ctx := context.Background()
	clk := dettest.NewClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	s := durabletest.NewStore()
	s.UseClock(clk)
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: t.TempDir(), Store: s, CreateMissing: true,
		Clock: clk, GC: durable.GCPolicy{Keep: 2, Grace: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.SetFaults(durabletest.Faults{}); h.Close() }()
	put := func(v string) {
		db, err := h.Get(ctx, "db-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.DB.Query(ctx, vactortest.Put(v)); err != nil {
			t.Fatal(err)
		}
	}
	s.SetFaults(durabletest.Faults{LandLate: time.Hour})
	for i := range vactor.MaxPendingPublishes {
		put(fmt.Sprint(i))
		h.WriteBack(ctx, "db-1")
	}
	clk.Advance(10 * time.Second)
	put("last")
	_, err = h.WriteBack(ctx, "db-1")
	if !errors.Is(err, vactor.ErrInDoubt) {
		t.Fatalf("write-back with publishes in doubt: %v, want ErrInDoubt", err)
	}
	if c := front.Classify(agentglue.AsUnavailable(err)); c != front.Unavailable {
		t.Fatalf("request path: %s, want unavailable", c)
	}
	if ce := classifyControl(classifyBackup(err)); ce == nil || ce.status != 503 || ce.code != "unavailable" {
		t.Fatalf("control/backup: %+v, want 503 unavailable", ce)
	}
	if got := retrySeconds(err); got != 20 {
		t.Fatalf("Retry-After %d s, want 20 (the oldest in doubt is past the 30 s grace in 20 s)", got)
	}
	if st := h.Stats(); st.PublishesInDoubt != 1 {
		t.Fatalf("PublishesInDoubt = %d, want 1", st.PublishesInDoubt)
	}
}
