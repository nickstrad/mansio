package vactortest_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det/dettest"

	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

// The fake must honour the same contract as the real S3 fetcher, or unit tests against it prove nothing.
func TestFakeFetcherContract(t *testing.T) {
	vactortest.RunFetcherSuite(t, func(t *testing.T) (vactor.Fetcher, func(string, []byte)) {
		f := vactortest.NewFetcher()
		return f, f.Put
	})
}

// Delay waits on the fetcher's Clock: the fetch ends when the simulated clock reaches it, and its timings are
// simulated durations.
func TestFetcherDelayWaitsOnItsClock(t *testing.T) {
	clk := dettest.NewClock(time.Unix(1000, 0))
	f := vactortest.NewFetcher()
	f.Put("k", []byte("abc"))
	f.Delay, f.Clock = time.Minute, clk
	type res struct {
		f   vactor.Fetched
		err error
	}
	done := make(chan res, 1)
	go func() {
		got, err := f.Fetch(context.Background(), "k", filepath.Join(t.TempDir(), "k"))
		done <- res{got, err}
	}()
	if !clk.AwaitPending(1) {
		t.Fatal("the fetch never waited on the clock")
	}
	select {
	case r := <-done:
		t.Fatalf("returned before the clock moved: %+v", r)
	default:
	}
	clk.Advance(time.Minute)
	if r := <-done; r.err != nil || r.f.Bytes != 3 || r.f.Total != time.Minute {
		t.Fatalf("Fetch = %+v, %v; want 3 bytes in exactly one simulated minute", r.f, r.err)
	}
}
