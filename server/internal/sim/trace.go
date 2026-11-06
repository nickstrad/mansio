package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"slices"
	"sync"
	"time"
)

// trace is the run's event log. Goroutines add lines during a step; flush (at quiescence) sorts the step's
// lines by stream, keeping each stream's own order, and appends them. So the order in which goroutines that
// one event woke happened to log does not change the trace, while anything they did differently does.
type trace struct {
	mu    sync.Mutex
	now   func() time.Time
	t0    time.Time
	step  []line
	lines []string
	sum   hash.Hash
	live  io.Writer // optional: every flushed line as it is flushed
}

type line struct {
	stream string
	text   string
}

func newTrace(t0 time.Time, now func() time.Time, live io.Writer) *trace {
	return &trace{now: now, t0: t0, sum: sha256.New(), live: live}
}

// add logs one line on stream, stamped with the simulated time.
func (t *trace) add(stream, format string, args ...any) {
	text := fmt.Sprintf("%10.6f %-10s %s", t.now().Sub(t.t0).Seconds(), stream, fmt.Sprintf(format, args...))
	t.mu.Lock()
	t.step = append(t.step, line{stream, text})
	t.mu.Unlock()
}

// flush ends a step. pre: the caller has let every goroutine settle.
func (t *trace) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	slices.SortStableFunc(t.step, func(a, b line) int {
		switch {
		case a.stream < b.stream:
			return -1
		case a.stream > b.stream:
			return 1
		}
		return 0
	})
	for _, l := range t.step {
		t.lines = append(t.lines, l.text)
		t.sum.Write([]byte(l.text))
		t.sum.Write([]byte{'\n'})
		if t.live != nil {
			fmt.Fprintln(t.live, l.text)
		}
	}
	t.step = t.step[:0]
}

func (t *trace) hash() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return hex.EncodeToString(t.sum.Sum(nil))
}

func (t *trace) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.lines)
}
