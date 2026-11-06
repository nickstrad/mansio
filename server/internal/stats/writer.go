// Package stats streams per-request records and resource samples to CSV files while the load runs.
//
// Producers never block on I/O: workers hand full record buffers to a writer goroutine through a bounded
// channel, and if the writer falls behind, the buffer is dropped and counted rather than stalling the load.
// The files are flushed every second, so they can be inspected (or loaded into DuckDB) mid-run.
//
// RecordFormat (the row layout) is pure; RequestWriter, CSV and Sampler do the I/O.
package stats

import (
	"bufio"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/outcome"
)

// Record is one request attempt. Times are nanoseconds; offsets are since the run started.
type Record struct {
	Phase      int16
	Client     uint8 // index into RecordFormat.Clients
	Attempt    uint8 // 1 = first try
	Final      bool  // no retry follows this attempt (it succeeded or the client gave up)
	DB         int32
	Class      uint8 // index into RecordFormat.Classes
	Write      bool
	ExpectSlow bool
	Outcome    outcome.Code
	Reopened   bool
	Batch      int16
	Rows       int32
	SchedNs    int64 // when the schedule wanted it sent
	SendNs     int64 // when it was handed to the actor
	LatencyNs  int64 // send → reply
	E2ENs      int64 // first attempt scheduled → reply (includes backlog and retries; avoids coordinated omission)
	QueueNs    int64
	ExecNs     int64
	CommitNs   int64
}

// RecordFormat is the requests.csv layout. Classes and Clients name Record.Class and Record.Client; names must
// not contain commas, quotes or newlines (class names are fixed; client names are checked by Validate).
type RecordFormat struct {
	Classes []string
	Clients []string
}

// recordColumns is the header; queries/report.sql and compare.sql depend on these names.
var recordColumns = []string{
	"phase", "client", "attempt", "final", "db", "class", "write", "expect_slow", "err", "reopened", "batch", "rows",
	"sched_ms", "send_ms", "latency_us", "e2e_us", "queue_us", "exec_us", "commit_us",
}

// Validate reports names that would break the unquoted CSV row. load.Config.Normalize rejects such client names
// before a run starts; NewRequestWriter asserts it.
func (f RecordFormat) Validate() error {
	for _, n := range slices.Concat(f.Classes, f.Clients) {
		if n == "" || strings.ContainsAny(n, ",\"\n\r") {
			return errs.Wrap("stats", nil, "name %q cannot be a CSV field (empty, or holds a comma, quote or newline)", n)
		}
	}
	return nil
}

// Header is the CSV header line, with its newline.
func (f RecordFormat) Header() string { return strings.Join(recordColumns, ",") + "\n" }

// Append appends r as one CSV line, with its newline.
//
//	pre: r.Class < len(Classes), r.Client < len(Clients)
func (f RecordFormat) Append(b []byte, r *Record) []byte {
	field := func(s string) { b = append(b, s...); b = append(b, ',') }
	num := func(v int64) { b = strconv.AppendInt(b, v, 10); b = append(b, ',') }
	flag := func(v bool) { b = strconv.AppendBool(b, v); b = append(b, ',') }
	frac := func(ns int64, unit float64, places int) {
		b = strconv.AppendFloat(b, float64(ns)/unit, 'f', places, 64)
		b = append(b, ',')
	}

	num(int64(r.Phase))
	field(f.Clients[r.Client])
	num(int64(r.Attempt))
	flag(r.Final)
	num(int64(r.DB))
	field(f.Classes[r.Class])
	flag(r.Write)
	flag(r.ExpectSlow)
	field(r.Outcome.String())
	flag(r.Reopened)
	num(int64(r.Batch))
	num(int64(r.Rows))
	frac(r.SchedNs, 1e6, 3)
	frac(r.SendNs, 1e6, 3)
	for _, v := range [...]int64{r.LatencyNs, r.E2ENs, r.QueueNs, r.ExecNs, r.CommitNs} {
		frac(v, 1e3, 1)
	}
	b[len(b)-1] = '\n' // the last comma ends the line
	return b
}

// Buffer collects records for one producer and hands them off when full or a second old. Not safe for
// concurrent use: give each producer its own.
type Buffer struct {
	w       *RequestWriter
	recs    []Record
	started time.Time
}

// Add appends r, handing the buffer off when it is full or older than a second (age is checked every 64
// records, so a quiet producer's records can wait longer; Flush when it stops).
func (b *Buffer) Add(r Record) {
	if len(b.recs) == 0 {
		b.started = time.Now()
	}
	b.recs = append(b.recs, r)
	if n := len(b.recs); n == cap(b.recs) || (n%64 == 0 && time.Since(b.started) > time.Second) {
		b.Flush()
	}
}

// Flush hands the buffer to the writer without blocking.
func (b *Buffer) Flush() {
	if len(b.recs) == 0 {
		return
	}
	b.w.submit(b.recs)
	b.recs = b.w.getBuf()
}

// RequestWriter writes requests.csv from a goroutine of its own.
type RequestWriter struct {
	format  RecordFormat
	f       *os.File
	bw      *bufio.Writer
	ch      chan []Record
	pool    sync.Pool
	done    chan struct{}
	written atomic.Int64
	dropped atomic.Int64
}

const bufRecords = 1024

// NewRequestWriter creates path, writes the header and starts the writer goroutine.
//
//	pre: format.Validate() == nil
func NewRequestWriter(path string, format RecordFormat) (*RequestWriter, error) {
	assert.That(format.Validate() == nil, "NewRequestWriter: %v", format.Validate())
	f, err := os.Create(path)
	if err != nil {
		return nil, errs.Wrap("stats", err, "cannot create %s", path)
	}
	w := &RequestWriter{format: format, f: f, bw: bufio.NewWriterSize(f, 1<<20), ch: make(chan []Record, 4096), done: make(chan struct{})}
	w.pool.New = func() any { return make([]Record, 0, bufRecords) }
	w.bw.WriteString(format.Header())
	go w.loop()
	return w, nil
}

// NewBuffer returns a buffer for one producer.
func (w *RequestWriter) NewBuffer() *Buffer { return &Buffer{w: w, recs: w.getBuf()} }

func (w *RequestWriter) getBuf() []Record { return w.pool.Get().([]Record)[:0] }

func (w *RequestWriter) submit(recs []Record) {
	select {
	case w.ch <- recs:
	default:
		w.dropped.Add(int64(len(recs)))
	}
}

// Written and Dropped count records.
func (w *RequestWriter) Written() int64 { return w.written.Load() }
func (w *RequestWriter) Dropped() int64 { return w.dropped.Load() }

func (w *RequestWriter) loop() {
	defer close(w.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	line := make([]byte, 0, 256)
	for {
		select {
		case recs, ok := <-w.ch:
			if !ok {
				return
			}
			for i := range recs {
				line = w.format.Append(line[:0], &recs[i])
				w.bw.Write(line)
			}
			w.written.Add(int64(len(recs)))
			w.pool.Put(recs[:0])
		case <-tick.C:
			w.bw.Flush()
		}
	}
}

// Close flushes and closes the file. Call it after every Buffer has been flushed.
func (w *RequestWriter) Close() error {
	close(w.ch)
	<-w.done
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return errs.Wrap("stats", err, "cannot write requests.csv")
	}
	return w.f.Close()
}
