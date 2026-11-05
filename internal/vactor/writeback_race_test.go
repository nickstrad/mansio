package vactor_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/vactortest"
)

// Regression (-race): one writer, while other goroutines Get, WriteBack, Passivate and Tick the same key and
// the store fails on and off. Every write the writer saw acknowledged must survive: after a final clean
// passivation the published generation holds the last acknowledged write.
func TestConcurrentWriteBackAndPassivateLoseNoAcknowledgedWrite(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) {
		o.Policy = vactor.Policy{WriteBackCommits: 3}
		o.Now = time.Now
	})
	const key = "t/hot"
	var lastAcked atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { // the writer
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			res, err := r.h.Get(ctx, key)
			if err != nil {
				continue // passivating with a failing store, etc.
			}
			if res.DB.Query(ctx, vactortest.Put(fmt.Sprint(i))) == nil {
				lastAcked.Store(i)
			}
		}
	})
	loop := func(fn func()) {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					fn()
				}
			}
		})
	}
	loop(func() { r.h.Get(ctx, key) })
	loop(func() { r.h.WriteBack(ctx, key) })
	loop(func() { r.h.Passivate(ctx, key); time.Sleep(100 * time.Microsecond) })
	loop(func() { r.h.Tick(ctx); time.Sleep(50 * time.Microsecond) })
	loop(func() {
		r.s.SetFaults(durabletest.Faults{FailUpload: true})
		time.Sleep(200 * time.Microsecond)
		r.s.SetFaults(durabletest.Faults{})
		time.Sleep(500 * time.Microsecond)
	})
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	r.s.SetFaults(durabletest.Faults{})
	if err := r.h.Passivate(ctx, key); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(lastAcked.Load())
	if lastAcked.Load() == 0 {
		t.Fatal("no write was ever acknowledged")
	}
	if got := r.published(key); got != want {
		t.Fatalf("published %q, last acknowledged write %q: an acknowledged write was lost", got, want)
	}
	if s := r.h.Stats(); s.WriteBacks < 2 {
		t.Logf("only %d write-backs; the race was mild", s.WriteBacks)
	}
}
