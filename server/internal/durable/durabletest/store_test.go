package durabletest_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
)

// The fake must honour the same contract as the S3 adapter, or unit tests against it prove nothing.
func TestFakeStoreContract(t *testing.T) {
	durabletest.RunStoreSuite(t, func(t *testing.T) durable.Store { return durabletest.NewStore() })
}

func TestFakeGCStoreContract(t *testing.T) {
	durabletest.RunGCStoreSuite(t, func(t *testing.T) durable.GCStore { return durabletest.NewStore() })
}

// A stall (Delay) waits on the store's clock: it ends when the simulated clock reaches it, and a deadline on
// that clock ends it early without applying anything.
func TestDelayWaitsOnTheStoreClock(t *testing.T) {
	clk := dettest.NewClock(time.Unix(1000, 0))
	s := durabletest.NewStore()
	s.UseClock(clk)
	s.SetFaults(durabletest.Faults{Delay: time.Minute})
	done := make(chan error, 1)
	go func() {
		_, _, err := s.ReadManifest(context.Background(), "k")
		done <- err
	}()
	if !clk.AwaitPending(1) {
		t.Fatal("the call never waited on the clock")
	}
	clk.Advance(time.Minute - time.Nanosecond)
	select {
	case err := <-done:
		t.Fatalf("returned before the delay ended: %v", err)
	default:
	}
	clk.Advance(time.Nanosecond)
	if err := <-done; !errors.Is(err, durable.ErrNoManifest) {
		t.Fatalf("after the delay: %v, want ErrNoManifest", err)
	}

	ctx, cancel := det.WithTimeout(context.Background(), clk, time.Second)
	defer cancel()
	go func() {
		_, err := s.PublishManifest(ctx, durable.Manifest{Key: "k", Generation: 0, Epoch: 1}, durable.NoVersion)
		done <- err
	}()
	clk.AwaitPending(2) // the ctx deadline and the stall
	clk.Advance(time.Second)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline inside the stall: %v, want DeadlineExceeded", err)
	}
	if _, ok := s.Manifest("k"); ok {
		t.Fatal("a call cut off during its stall applied")
	}
	if m := s.Seed("k", 1, []byte("x")); !m.WrittenAt.Equal(clk.Now()) {
		t.Fatalf("Seed stamped %v, want the store clock's %v", m.WrittenAt, clk.Now())
	}
}

// SetCallFaults decides each call's switches from its op and key, on top of SetFaults.
func TestCallFaultsDecidePerCall(t *testing.T) {
	s := durabletest.NewStore()
	s.SetFaults(durabletest.Faults{FailRead: true})
	var seen []string
	s.SetCallFaults(func(op, key string, static durabletest.Faults) durabletest.Faults {
		seen = append(seen, op+" "+key)
		static.FailRead = static.FailRead && key == "bad"
		static.LosePublishReply = op == durable.OpPublish
		return static
	})
	ctx := context.Background()
	if _, _, err := s.ReadManifest(ctx, "bad"); !errors.Is(err, durabletest.ErrInjected) {
		t.Fatalf("bad key read: %v, want injected", err)
	}
	if _, _, err := s.ReadManifest(ctx, "good"); !errors.Is(err, durable.ErrNoManifest) {
		t.Fatalf("good key read: %v, want ErrNoManifest", err)
	}
	if _, err := s.PublishManifest(ctx, durable.Manifest{Key: "good", Epoch: 1}, durable.NoVersion); !errors.Is(err, durabletest.ErrInjected) {
		t.Fatalf("publish: %v, want a lost reply", err)
	}
	if _, ok := s.Manifest("good"); !ok {
		t.Fatal("a lost-reply publish did not apply")
	}
	want := []string{durable.OpGetManifest + " bad", durable.OpGetManifest + " good", durable.OpPublish + " good"}
	if !slices.Equal(seen, want) {
		t.Fatalf("CallFaults saw %q, want %q", seen, want)
	}
	s.SetCallFaults(nil)
	if _, _, err := s.ReadManifest(ctx, "good"); !errors.Is(err, durabletest.ErrInjected) {
		t.Fatalf("after SetCallFaults(nil): %v, want the static FailRead", err)
	}
}
