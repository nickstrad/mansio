package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"sync"
	"testing"

	"github.com/nickstrad/mansio/server/internal/coldload"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

func rec(seq int) coldload.Record {
	lv := coldload.Level{Kind: coldload.KindCold, Conc: 1, Keys: make([]string, 1000)}
	lv.Keys[seq] = "k,with,commas"
	return coldload.Record{Level: lv, Seq: seq, Res: vactor.Result{Act: &vactor.Activation{Bytes: 1}}}
}

func TestRecorderWritesEveryRecordFromManyGoroutines(t *testing.T) {
	var buf bytes.Buffer
	r := newRecorder(&buf, 1<<16)
	const workers, per = 20, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range per {
				r.Add(rec(i))
			}
		})
	}
	wg.Wait()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v", err)
	}
	if len(rows) != 1+workers*per || r.Dropped() != 0 {
		t.Fatalf("rows = %d (+header), dropped = %d; want %d, 0", len(rows)-1, r.Dropped(), workers*per)
	}
	if rows[0][0] != "level" || rows[1][5] != "k,with,commas" {
		t.Fatalf("header %q / row %q", rows[0], rows[1])
	}
}

func TestRecorderDropsWhenFullInsteadOfBlocking(t *testing.T) {
	tests := []struct {
		buffer, adds int
		wantDropped  int64
	}{
		{0, 1, 1}, // no room at all
		{1, 1, 0}, // on point: exactly fits
		{1, 3, 2}, // over by two
	}
	for _, tc := range tests {
		var buf bytes.Buffer
		r := makeRecorder(&buf, tc.buffer) // loop not started yet: the channel is the only room
		for i := range tc.adds {
			r.Add(rec(i))
		}
		if r.Dropped() != tc.wantDropped {
			t.Fatalf("buffer %d, %d adds: dropped %d, want %d", tc.buffer, tc.adds, r.Dropped(), tc.wantDropped)
		}
		go r.loop()
		if err := r.Close(); err != nil { // Close also asserts written + dropped == added
			t.Fatal(err)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestRecorderCloseReportsWriteError(t *testing.T) {
	r := newRecorder(failWriter{}, 4)
	r.Add(rec(0))
	if err := r.Close(); err == nil {
		t.Fatal("Close hid a write error")
	}
}
