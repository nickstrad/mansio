package det_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/det/dettest"
)

func TestNewRandIsReproducible(t *testing.T) {
	a, b, c := rand.New(det.NewRand(7)), rand.New(det.NewRand(7)), rand.New(det.NewRand(8))
	same, differs := true, false
	for range 100 {
		x, y, z := a.Uint64(), b.Uint64(), c.Uint64()
		same = same && x == y
		differs = differs || x != z
	}
	if !same || !differs {
		t.Fatalf("same seed equal = %v, other seed differs = %v", same, differs)
	}
}

func TestIDs(t *testing.T) {
	format := regexp.MustCompile(`^c[0-9a-f]{12}-[0-9a-z]+$`)
	sys := det.SystemIDs("c")
	if a, b := sys.NewID(), sys.NewID(); !format.MatchString(a) || a == b || a[:14] != b[:14] {
		t.Fatalf("system ids %q, %q: want one prefix, distinct counters", a, b)
	}
	if a, b := det.SystemIDs("c").NewID(), det.SystemIDs("c").NewID(); a == b {
		t.Fatalf("two generators made the same id %q", a)
	}
	g1, g2 := det.NewIDs("c", det.NewRand(1)), det.NewIDs("c", det.NewRand(1))
	for i := range 40 {
		if a, b := g1.NewID(), g2.NewID(); a != b || !format.MatchString(a) {
			t.Fatalf("id %d: %q vs %q: want equal ids of the system format from one seed", i, a, b)
		}
	}
	if got := g1.NewID(); got[len(got)-2:] != "15" { // the 41st id: 41 in base 36
		t.Fatalf("41st id %q", got)
	}
}

func TestOrDefaults(t *testing.T) {
	if det.Or(nil) == nil || det.RandOr(nil) == nil {
		t.Fatal("nil defaults")
	}
	clk := dettest.NewClock(time.Unix(0, 0))
	if det.Or(clk) != det.Clock(clk) {
		t.Fatal("Or replaced a set clock")
	}
}

func TestSystemClockDeadline(t *testing.T) {
	c := det.System()
	ctx, cancel := det.WithTimeout(context.Background(), c, time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("err %v", ctx.Err())
	}
	if err := det.Sleep(context.Background(), c, time.Millisecond); err != nil {
		t.Fatalf("Sleep = %v", err)
	}
}

func TestSleep(t *testing.T) {
	clk := dettest.NewClock(time.Unix(100, 0))
	done := make(chan error, 1)
	go func() { done <- det.Sleep(context.Background(), clk, time.Second) }()
	if !clk.AwaitPending(1) {
		t.Fatal("Sleep did not arm a timer")
	}
	clk.Advance(999 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Sleep returned %v before its time", err)
	default:
	}
	clk.Advance(time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("Sleep = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := det.Sleep(ctx, clk, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep on a canceled ctx = %v", err)
	}
	if err := det.Sleep(context.Background(), clk, 0); err != nil {
		t.Fatalf("Sleep(0) = %v", err)
	}
}
