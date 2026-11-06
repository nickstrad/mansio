package coldload

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// Getter is the port to the virtual-actor host.
type Getter interface {
	Get(ctx context.Context, key string) (vactor.Result, error)
}

// Record is one Get as the runner saw it. Exactly one Record is produced per key that was dispatched.
type Record struct {
	Level Level
	Seq   int           // index into Level.Keys
	Start time.Duration // level start → this Get started
	Wait  time.Duration // this Get's own duration
	Res   vactor.Result
	Err   error
}

// LevelResult summarises one level.
type LevelResult struct {
	Dispatched int // Gets started (all keys unless ctx ended early)
	OK, Failed int
	Bytes      int64 // fetched by the activations this level started (local hits excluded)
	Wall       time.Duration
}

// RunLevel calls g.Get for every key of lv with lv.Conc in flight and hands each Record to sink (from the
// worker goroutines: sink must be safe for concurrent use and must not block). It stops dispatching when
// ctx ends. Workers send their outcome to the parent and decide nothing (Cox-Buday ch. 4); a panic in a
// worker is forwarded and re-raised here.
//
//	pre:  lv.Conc ≥ 1
//	post: Dispatched = OK + Failed; Dispatched = len(lv.Keys) unless ctx ended
func RunLevel(ctx context.Context, g Getter, lv Level, sink func(Record)) LevelResult {
	assert.That(lv.Conc >= 1, "RunLevel: level %d has Conc %d", lv.Index, lv.Conc)
	type outcome struct {
		rec   Record
		panic any
	}
	workers := min(lv.Conc, len(lv.Keys))
	jobs := make(chan int)
	outs := make(chan outcome, workers) // drained by the parent while workers run
	t0 := time.Now()
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				out := func() (o outcome) {
					defer func() {
						if p := recover(); p != nil {
							o = outcome{panic: fmt.Sprintf("coldload: Get(%s) panicked: %v\n%s", lv.Keys[i], p, debug.Stack())}
						}
					}()
					start := time.Since(t0)
					res, err := g.Get(ctx, lv.Keys[i])
					return outcome{rec: Record{Level: lv, Seq: i, Start: start, Wait: time.Since(t0) - start, Res: res, Err: err}}
				}()
				outs <- out
			}
		})
	}
	go func() {
		defer close(jobs)
		for i := range lv.Keys {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(outs) }()

	var r LevelResult
	var firstPanic any
	for o := range outs { // read every outcome, even after a panic, so nothing is lost or left blocked
		if o.panic != nil {
			if firstPanic == nil {
				firstPanic = o.panic
			}
			continue
		}
		r.Dispatched++
		if o.rec.Err != nil {
			r.Failed++
		} else {
			r.OK++
			if a := o.rec.Res.Act; a != nil && !a.LocalHit {
				r.Bytes += a.Bytes
			}
		}
		sink(o.rec)
	}
	r.Wall = time.Since(t0)
	if firstPanic != nil {
		panic(firstPanic)
	}
	assert.That(r.Dispatched == r.OK+r.Failed, "RunLevel: %d dispatched != %d ok + %d failed", r.Dispatched, r.OK, r.Failed)
	assert.That(ctx.Err() != nil || r.Dispatched == len(lv.Keys), "RunLevel: %d of %d keys dispatched", r.Dispatched, len(lv.Keys))
	return r
}
