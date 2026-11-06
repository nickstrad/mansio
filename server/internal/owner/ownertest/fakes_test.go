package ownertest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

func TestMemStoreContract(t *testing.T) {
	ownertest.RunLocalStoreSuite(t, func(t *testing.T) (owner.LocalStore, func() owner.LocalStore) {
		m := ownertest.NewMemStore()
		return m, func() owner.LocalStore { return m }
	})
}

// The fake object store must implement contract 1 exactly, or every test built on it proves nothing.
func TestObjectsImplementContract1(t *testing.T) {
	o := ownertest.NewObjects()
	g1, _ := o.Upload("k", []string{"a"})
	if err := o.Publish("k", 0, g1, 1); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, _, v, _, _ := o.Read("k")
	g2, _ := o.Upload("k", []string{"a", "b"})
	// Off point: version read before the last publish → changed since read.
	if err := o.Publish("k", v-1, g2, 1); !errors.Is(err, owner.ErrStale) {
		t.Fatalf("publish with an old read = %v, want ErrStale", err)
	}
	// On point: same epoch, current version → ok.
	if err := o.Publish("k", v, g2, 1); err != nil {
		t.Fatalf("publish at same epoch: %v", err)
	}
	_, _, v, _, _ = o.Read("k")
	if err := o.Publish("k", v, g2, 5); err != nil { // higher epoch → ok
		t.Fatalf("publish at higher epoch: %v", err)
	}
	_, _, v, _, _ = o.Read("k")
	g3, _ := o.Upload("k", []string{"zombie"})
	if err := o.Publish("k", v, g3, 4); !errors.Is(err, owner.ErrStale) { // off point: epoch 4 < stored 5
		t.Fatalf("lower-epoch publish = %v, want ErrStale", err)
	}
	gen, epoch, _, contents, _ := o.Read("k")
	if gen != g2 || epoch != 5 || len(contents) != 2 {
		t.Fatalf("manifest = g%d e%d %v, want g%d e5 [a b]", gen, epoch, contents, g2)
	}
}

func TestWriteBackStampConditions(t *testing.T) {
	ctx := context.Background()
	o := ownertest.NewObjects()
	wb := &ownertest.WriteBack{Objects: o, Disk: ownertest.NewDisk()}
	if _, err := wb.Head(ctx, "k"); !errors.Is(err, owner.ErrNoManifest) {
		t.Fatalf("Head(missing) = %v", err)
	}
	if err := wb.Stamp(ctx, "k", 1, 3); !errors.Is(err, owner.ErrStale) { // no manifest, gen ≠ 0
		t.Fatalf("Stamp(missing, gen 3) = %v, want ErrStale", err)
	}
	if err := wb.Stamp(ctx, "k", 1, 0); err != nil { // no manifest, gen 0 creates
		t.Fatalf("Stamp(missing, gen 0) = %v", err)
	}
	wb.Disk.Append("k", "w1")
	g, err := wb.Publish(ctx, "k", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.Stamp(ctx, "k", 2, g-1); !errors.Is(err, owner.ErrStale) { // gen moved on
		t.Fatalf("Stamp(old gen) = %v, want ErrStale", err)
	}
	if err := wb.Stamp(ctx, "k", 2, g); err != nil {
		t.Fatal(err)
	}
	if _, err := wb.Publish(ctx, "k", 1); !errors.Is(err, owner.ErrStale) { // the backstop
		t.Fatalf("epoch-1 publish after the epoch-2 stamp = %v, want ErrStale", err)
	}
}

// TestPlacementCAS runs the Placement contract suite on the fake (the Postgres adapter runs it too:
// internal/placement/pg), then checks the fake's fault switches.
func TestPlacementCAS(t *testing.T) {
	ownertest.RunPlacementSuite(t, func(*testing.T) owner.Placement { return ownertest.NewPlacement() })
	t.Run("fault switches", testPlacementSwitches)
}

func testPlacementSwitches(t *testing.T) {
	ctx := context.Background()
	p := ownertest.NewPlacement()
	a1 := owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive}
	if err := p.CAS(ctx, "k", owner.Place{}, a1); err != nil {
		t.Fatal(err)
	}
	if err := p.CAS(ctx, "k", owner.Place{}, a1); !errors.Is(err, owner.ErrConflict) {
		t.Fatalf("CAS with a wrong expect = %v, want ErrConflict", err)
	}
	if err := p.CAS(ctx, "k", a1, owner.Place{Owner: "b", Epoch: 1, State: owner.PlaceActive}); err == nil {
		t.Fatal("owner change without an epoch bump was accepted")
	}
	p.SetStale(true)
	b2 := owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive}
	if err := p.CAS(ctx, "k", a1, b2); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(ctx, "k"); got != a1 {
		t.Fatalf("stale Get = %+v, want the frozen %+v", got, a1)
	}
	p.SetStale(false)
	p.SetLoseReply(true)
	b3 := owner.Place{Owner: "b", Epoch: 3, State: owner.PlaceActive}
	if err := p.CAS(ctx, "k", b2, b3); !errors.Is(err, ownertest.ErrDown) {
		t.Fatalf("lost reply = %v", err)
	}
	if got, _ := p.Get(ctx, "k"); got != b3 {
		t.Fatalf("a lost reply must still apply: %+v", got)
	}
	p.SetDown(true)
	if _, err := p.Get(ctx, "k"); !errors.Is(err, ownertest.ErrDown) {
		t.Fatalf("down Get = %v", err)
	}
}

// Delay waits on the placement's clock, and a deadline on that clock cuts it short.
func TestPlacementDelayWaitsOnItsClock(t *testing.T) {
	clk := dettest.NewClock(time.Unix(1000, 0))
	p := ownertest.NewPlacement()
	p.UseClock(clk)
	p.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
	p.SetDelay(time.Minute)
	done := make(chan error, 1)
	go func() {
		_, err := p.Get(context.Background(), "k")
		done <- err
	}()
	if !clk.AwaitPending(1) {
		t.Fatal("Get never waited on the clock")
	}
	clk.Advance(time.Minute)
	if err := <-done; err != nil {
		t.Fatalf("Get after the delay: %v", err)
	}
	ctx, cancel := det.WithTimeout(context.Background(), clk, time.Second)
	defer cancel()
	go func() {
		_, err := p.Get(ctx, "k")
		done <- err
	}()
	clk.AwaitPending(2)
	clk.Advance(time.Second)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get past its deadline: %v", err)
	}
}
