// Package eventlog writes mansio's event log: the JSON-lines records of core/events.ts (envelope, the kinds
// http, handler, sql, tx, queue, backup and lifecycle, the same redaction) under
// <dir>/<role>/<YYYY-MM-DD>/events-<HH>-<pid>-<seq>.jsonl, the layout the TS ingest reads
// (core/eventlog/offsets.ts). The DuckDB event log and its analysis catalog are offline steps on those files
// (deno task logs:ingest, logs:query; ADR 0004, 0007): Go only writes JSONL.
//
//	Emit ─(never blocks)─> bounded queue ─> one writer goroutine ─> buffered file, rolled on date, hour, size
//	        full: counted                     each drain: a "queue" record for the drops, then the batch, flush
//
// Release It B1, B2: the request path never waits on the log. A full queue drops the record and counts it;
// the writer reports the count as its own queue record (queue "events", event "dropped") before the next
// batch, as platform/logging/logger.ts does, and the count is exposed in Stats. Write failures lose the
// records (counted) and are reported to the operational log at most once a minute.
//
// Role: adapter (files). Error module "eventlog".
package eventlog

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
)

const module = "eventlog"

// Defaults (core/logging/rotation.ts LOG_MAX_FILE_BYTES; the TS sink's queue).
const (
	DefaultMaxFileBytes = 64 << 20
	DefaultBuffer       = 8192
	// SinkTraceID is the trace id of the log's own drop reports (platform/logging/logger.ts SINK_TRACE_ID).
	SinkTraceID = "event-sink"
)

// Config is where and how the log writes.
type Config struct {
	Dir          string // LOG_DIR: the parent of the role directories (required)
	Role         string // "server" (agentd), "reader", "restore" or "cli" (required)
	Level        Level  // records below it are not queued; drop reports ignore it
	Buffer       int    // queue capacity; default DefaultBuffer
	MaxFileBytes int64  // roll after this many bytes; default DefaultMaxFileBytes
	PID          int    // default os.Getpid()
	// Now is the clock for ts and file names (local date and hour, as the TS sink); default time.Now.
	Now func() time.Time
	// Log receives the degradation reports (EVENTLOG.WRITE_FAILED), at most one a minute; nil = none.
	Log *slog.Logger
}

// Stats are the log's counters since Open.
type Stats struct {
	Written int64 // records written
	Dropped int64 // records refused because the queue was full
	Lost    int64 // records taken but not written (a write failed)
	Depth   int   // records queued now
	Cap     int
}

// Log is an open event log. Emit is safe for concurrent use.
type Log struct {
	cfg  Config
	ch   chan Event
	done chan struct{}

	mu     sync.RWMutex // held for reading by Emit, for writing by Close: no send after close(ch)
	closed bool

	written, dropped, lost atomic.Int64
	reported               int64 // dropped already reported (writer only)

	f        *os.File
	w        *bufio.Writer
	buffered int64 // records in w not yet flushed
	path     string
	dayHour  string
	size     int64
	lastWarn time.Time

	beforeWrite func() // tests: runs before each record is written (stalls the writer)
}

// Open starts a log. Nothing is written until the first record.
func Open(cfg Config) (*Log, error) { return open(cfg, nil) }

func open(cfg Config, beforeWrite func()) (*Log, error) {
	switch cfg.Role {
	case "server", "reader", "restore", "cli":
	default:
		return nil, errs.Wrap(module, nil, "role %q is not one of server, reader, restore, cli", cfg.Role)
	}
	if cfg.Dir == "" {
		return nil, errs.Wrap(module, nil, "the log directory is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, errs.Wrap(module, err, "cannot create the event log directory").With("dir", cfg.Dir)
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultBuffer
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = DefaultMaxFileBytes
	}
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	l := &Log{cfg: cfg, ch: make(chan Event, cfg.Buffer), done: make(chan struct{}), beforeWrite: beforeWrite}
	go l.run()
	return l, nil
}

// Emit queues ev and returns at once. A record below the level is skipped; a full queue drops it (counted).
// After Close, Emit drops (counted) too.
func (l *Log) Emit(ev Event) {
	if l == nil || ev.Payload == nil || ev.Level < l.cfg.Level {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = l.cfg.Now()
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		l.dropped.Add(1)
		return
	}
	select {
	case l.ch <- ev:
	default:
		l.dropped.Add(1)
	}
}

// Stats returns the counters.
func (l *Log) Stats() Stats {
	return Stats{Written: l.written.Load(), Dropped: l.dropped.Load(), Lost: l.lost.Load(), Depth: len(l.ch), Cap: cap(l.ch)}
}

// Close stops accepting records, writes what is queued, and syncs and closes the file, giving up when ctx
// ends (the rest is lost). It is safe to call more than once.
func (l *Log) Close(ctx context.Context) error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.ch)
	}
	l.mu.Unlock()
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return errs.Wrap(module, ctx.Err(), "the event log did not finish writing in time; queued records are lost")
	}
}

// run is the writer: it writes each record after a report of any drops before it, and flushes whenever the
// queue is empty, so a trickle reaches the file at once and a burst goes out in large writes.
func (l *Log) run() {
	defer close(l.done)
	defer l.closeFile()
	for ev := range l.ch {
		l.reportDrops()
		l.write(ev)
		if len(l.ch) == 0 {
			l.reportDrops()
			l.flush()
		}
	}
	l.reportDrops()
}

// reportDrops writes one queue record for the drops not reported yet. Drop reports bypass the queue (and the
// level), so the saturation they describe cannot drop them.
func (l *Log) reportDrops() {
	n := l.dropped.Load()
	if n == l.reported {
		return
	}
	if l.write(Event{Level: Warn, Time: l.cfg.Now(), TraceID: SinkTraceID,
		Payload: Queue{Queue: "events", Depth: len(l.ch), Cap: cap(l.ch), Event: "dropped", Dropped: n - l.reported}}) {
		l.reported = n
	}
}

// write buffers one record, rolling the file first if needed. It reports whether the record was taken; it
// counts as written once a flush puts it in the file.
func (l *Log) write(ev Event) bool {
	if l.beforeWrite != nil {
		l.beforeWrite()
	}
	line := stamp{EventID: newUUID(), TS: ev.Time, Role: l.cfg.Role, PID: l.cfg.PID}.line(ev)
	if err := l.roll(ev.Time, int64(len(line))+1); err != nil {
		l.lost.Add(1)
		l.fail(err)
		return false
	}
	l.w.Write(line)
	l.buffered++
	if err := l.w.WriteByte('\n'); err != nil { // bufio's errors stick: a failed auto-flush shows here
		l.fail(errs.Wrap(module, err, "cannot write the event log").With("path", l.path))
		return false
	}
	l.size += int64(len(line)) + 1
	return true
}

// roll opens the file for a record at t: a new one on a new local date or hour, or when this one is full.
func (l *Log) roll(t time.Time, n int64) error {
	local := t.In(time.Local)
	dayHour := local.Format("2006-01-02T15")
	if l.w != nil && dayHour == l.dayHour && (l.size == 0 || l.size+n <= l.cfg.MaxFileBytes) {
		return nil
	}
	l.closeFile()
	dir := filepath.Join(l.cfg.Dir, l.cfg.Role, local.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(module, err, "cannot create the event log directory").With("dir", dir)
	}
	hour := local.Format("15")
	seq, err := nextSeq(dir, hour, l.cfg.PID)
	if err != nil {
		return err
	}
	for {
		path := filepath.Join(dir, fmt.Sprintf("events-%s-%d-%03d.jsonl", hour, l.cfg.PID, seq))
		// O_EXCL: never append to a file another incarnation with this pid wrote (a container's pid 1
		// restarting within the hour), whose torn last line would merge with ours.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, fs.ErrExist) {
			seq++
			continue
		}
		if err != nil {
			return errs.Wrap(module, err, "cannot create the event log file").With("path", path)
		}
		l.f, l.w, l.path, l.dayHour, l.size = f, bufio.NewWriterSize(f, 64<<10), path, dayHour, 0
		return nil
	}
}

// nextSeq is one past the highest seq of events-<hour>-<pid>-<seq>.jsonl[.gz] in dir, or 1
// (platform/logging/file_sink.ts nextSeq): scanning, not counting, is what makes a restart with a recycled
// pid safe.
func nextSeq(dir, hour string, pid int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, errs.Wrap(module, err, "cannot list the event log directory").With("dir", dir)
	}
	pattern := regexp.MustCompile(`^events-` + hour + `-` + strconv.Itoa(pid) + `-(\d{3,})\.jsonl(\.gz)?$`)
	highest := 0
	for _, e := range entries {
		if m := pattern.FindStringSubmatch(e.Name()); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				highest = max(highest, n)
			}
		}
	}
	return highest + 1, nil
}

// flush writes the buffered records to the file: written on success, lost on failure.
func (l *Log) flush() {
	if l.w == nil {
		return
	}
	if err := l.w.Flush(); err != nil {
		l.fail(errs.Wrap(module, err, "cannot write the event log").With("path", l.path))
		return
	}
	l.written.Add(l.buffered)
	l.buffered = 0
}

func (l *Log) closeFile() {
	if l.f == nil {
		return
	}
	l.flush()
	if l.f == nil { // the flush failed and dropped the file
		return
	}
	if err := l.f.Sync(); err != nil {
		l.fail(errs.Wrap(module, err, "cannot sync the event log").With("path", l.path))
		return
	}
	l.f.Close()
	l.f, l.w = nil, nil
}

// fail counts the buffered records lost, drops the file (the next record opens a new one), and reports at
// most once a minute.
func (l *Log) fail(err error) {
	l.lost.Add(l.buffered)
	l.buffered = 0
	if l.f != nil {
		l.f.Close()
		l.f, l.w = nil, nil
	}
	now := l.cfg.Now()
	if l.cfg.Log == nil || now.Sub(l.lastWarn) < time.Minute {
		return
	}
	l.lastWarn = now
	l.cfg.Log.Warn("event log write failed; records are being lost", "event", "EVENTLOG.WRITE_FAILED",
		"error", err.Error(), "chain", errs.Chain(err), "lost", l.lost.Load())
}

// NewID returns a random UUID (version 4), as crypto.randomUUID: event, span and generated trace ids.
func NewID() string { return newUUID() }

func newUUID() string {
	var b [16]byte
	rand.Read(b[:]) // never fails (Go 1.24+)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j], out[j+1] = hex[c>>4], hex[c&0xf]
		j += 2
	}
	return string(out)
}
