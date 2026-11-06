package main

import (
	"bufio"
	"encoding/csv"
	"io"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/coldload"
)

// recorder writes activations.csv off the measured path: Add is a non-blocking channel send; one goroutine
// formats rows, writes them and flushes every second. A full channel drops the record and counts it, so an
// activation never waits for disk.
//
//	invariant (checked at Close): written + dropped == added
type recorder struct {
	ch                      chan coldload.Record
	done                    chan struct{}
	w                       io.Writer
	err                     error // first write error; owned by loop until done is closed
	added, written, dropped atomic.Int64
}

// newRecorder starts a recorder writing CSV to w.
func newRecorder(w io.Writer, buffer int) *recorder {
	r := makeRecorder(w, buffer)
	go r.loop()
	return r
}

// makeRecorder builds a recorder without starting its goroutine (tests use it to fill the buffer).
func makeRecorder(w io.Writer, buffer int) *recorder {
	return &recorder{ch: make(chan coldload.Record, buffer), done: make(chan struct{}), w: w}
}

func (r *recorder) Add(rec coldload.Record) {
	r.added.Add(1)
	select {
	case r.ch <- rec:
	default:
		r.dropped.Add(1)
	}
}

func (r *recorder) Dropped() int64 { return r.dropped.Load() }

// Close writes what is queued, flushes, and returns the first write error. Add must not be called after.
func (r *recorder) Close() error {
	close(r.ch)
	<-r.done
	assert.That(r.err != nil || r.written.Load()+r.dropped.Load() == r.added.Load(),
		"recorder: written %d + dropped %d != added %d", r.written.Load(), r.dropped.Load(), r.added.Load())
	return r.err
}

func (r *recorder) loop() {
	defer close(r.done)
	bw := bufio.NewWriterSize(r.w, 1<<20)
	w := csv.NewWriter(bw)
	flush := func() {
		w.Flush()
		if err := w.Error(); err != nil && r.err == nil {
			r.err = err
		}
		if err := bw.Flush(); err != nil && r.err == nil {
			r.err = err
		}
	}
	w.Write(coldload.ActivationHeader)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case rec, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			w.Write(rec.Row())
			r.written.Add(1)
		case <-tick.C:
			flush()
		}
	}
}
