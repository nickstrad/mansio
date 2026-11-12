package eventlog

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sourcePattern is the documented file layout (docs/observability.md "Files"): what log readers and collectors match.
var sourcePattern = regexp.MustCompile(`^(server|reader|restore|cli)/(\d{4}-\d{2}-\d{2})/events-(\d{2})-(\d+)-(\d{3,})\.jsonl$`)

// readAll returns every record under dir, by file (relative path) in name order.
func readAll(t *testing.T, dir string) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("%s: %v: %s", rel, err, sc.Text())
			}
			out[filepath.ToSlash(rel)] = append(out[filepath.ToSlash(rel)], rec)
		}
		return nil
	})
	return out
}

func closeLog(t *testing.T, l *Log) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func lifecycle(phase string) Event { return Event{TraceID: "t", Payload: Lifecycle{Phase: phase}} }

func TestWritesTheTSLayoutAndRolls(t *testing.T) {
	dir := t.TempDir()
	var now atomic.Pointer[time.Time]
	set := func(tm time.Time) { now.Store(&tm) }
	set(time.Date(2026, 11, 6, 9, 59, 0, 0, time.Local))
	lineBytes := int64(len(stamp{EventID: NewID(), TS: *now.Load(), Role: "server", PID: 77}.line(lifecycle("a")))) + 1
	l, err := Open(Config{Dir: dir, Role: "server", PID: 77, MaxFileBytes: 2*lineBytes + lineBytes/2, Now: func() time.Time { return *now.Load() }})
	if err != nil {
		t.Fatal(err)
	}
	l.Emit(lifecycle("a"))
	l.Emit(lifecycle("b"))
	l.Emit(lifecycle("c")) // the third rolls on size
	set(time.Date(2026, 11, 6, 10, 0, 0, 0, time.Local))
	l.Emit(lifecycle("d")) // a new hour
	set(time.Date(2026, 11, 7, 10, 0, 0, 0, time.Local))
	l.Emit(lifecycle("e")) // a new date
	closeLog(t, l)

	files := readAll(t, dir)
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range files {
			if !yield(k) {
				return
			}
		}
	})
	want := []string{"server/2026-11-06/events-09-77-001.jsonl", "server/2026-11-06/events-09-77-002.jsonl",
		"server/2026-11-06/events-10-77-001.jsonl", "server/2026-11-07/events-10-77-001.jsonl"}
	if !slices.Equal(names, want) {
		t.Fatalf("files %v, want %v", names, want)
	}
	for _, n := range names {
		if !sourcePattern.MatchString(n) {
			t.Errorf("%s is not a TS ingest source path", n)
		}
	}
	var phases []string
	for _, n := range names {
		for _, r := range files[n] {
			phases = append(phases, r["phase"].(string))
			if r["role"] != "server" || r["pid"] != 77.0 || r["kind"] != "lifecycle" || len(r["eventId"].(string)) != 36 {
				t.Errorf("envelope %v", r)
			}
		}
	}
	if strings.Join(phases, "") != "abcde" {
		t.Errorf("records out of order or lost: %v", phases)
	}
	if st := l.Stats(); st.Written != 5 || st.Dropped != 0 || st.Lost != 0 {
		t.Errorf("stats %+v", st)
	}
}

// TestEmitNeverBlocks: with the writer stalled, Emit returns at once; the drops are counted and reported as one
// queue record before the next batch (platform/logging/logger.ts decision 2).
func TestEmitNeverBlocks(t *testing.T) {
	dir := t.TempDir()
	stall, stalled := make(chan struct{}), make(chan struct{}, 1)
	var first atomic.Bool
	l, err := open(Config{Dir: dir, Role: "server", Buffer: 2}, func() {
		if first.CompareAndSwap(false, true) {
			stalled <- struct{}{}
			<-stall
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	l.Emit(lifecycle("taken")) // the writer takes it and stalls
	<-stalled
	start := time.Now()
	for range 10 {
		l.Emit(lifecycle("queued or dropped"))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Emit blocked for %v", d)
	}
	if st := l.Stats(); st.Dropped != 8 || st.Depth != 2 {
		t.Fatalf("stats %+v, want 8 dropped, 2 queued", st)
	}
	close(stall)
	closeLog(t, l)
	var recs []map[string]any
	for _, f := range readAll(t, dir) {
		recs = append(recs, f...)
	}
	if len(recs) != 4 {
		t.Fatalf("%d records, want taken + drop report + 2 queued: %v", len(recs), recs)
	}
	report := recs[1]
	if report["kind"] != "queue" || report["queue"] != "events" || report["event"] != "dropped" || report["dropped"] != 8.0 ||
		report["cap"] != 2.0 || report["traceId"] != SinkTraceID || report["level"] != "warn" {
		t.Errorf("drop report %v", report)
	}
}

func TestLevelGateAndClosedLog(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(Config{Dir: dir, Role: "server", Level: Warn})
	if err != nil {
		t.Fatal(err)
	}
	l.Emit(lifecycle("info: skipped"))
	l.Emit(Event{Level: Error, TraceID: "t", Payload: Lifecycle{Phase: "error: kept"}})
	closeLog(t, l)
	l.Emit(lifecycle("after close"))
	l.Emit(Event{Level: Error, Payload: Lifecycle{Phase: "after close"}})
	closeLog(t, l) // twice is fine
	n := 0
	for _, f := range readAll(t, dir) {
		n += len(f)
	}
	if st := l.Stats(); n != 1 || st.Written != 1 || st.Dropped != 1 {
		t.Errorf("%d records, stats %+v; want 1 written, the error after close dropped", n, st)
	}
	var nilLog *Log
	nilLog.Emit(lifecycle("nil log: no-op"))
}

func TestOpenRefusesBadConfig(t *testing.T) {
	if _, err := Open(Config{Dir: t.TempDir(), Role: "agent"}); err == nil {
		t.Error("role agent accepted: the TS ingest would never read it")
	}
	if _, err := Open(Config{Role: "server"}); err == nil {
		t.Error("no dir accepted")
	}
}

// TestWriteFailureLosesAndRecovers: an unwritable directory loses records (counted), and a later record
// after the directory is fixed is written.
func TestWriteFailureLosesAndRecovers(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().Format("2006-01-02")
	block := filepath.Join(dir, "server", day)
	os.MkdirAll(filepath.Join(dir, "server"), 0o755)
	os.WriteFile(block, []byte("a file where the date directory goes"), 0o644)
	l, err := Open(Config{Dir: dir, Role: "server"})
	if err != nil {
		t.Fatal(err)
	}
	l.Emit(lifecycle("lost"))
	deadline := time.Now().Add(5 * time.Second)
	for l.Stats().Lost == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	os.Remove(block)
	l.Emit(lifecycle("written"))
	closeLog(t, l)
	if st := l.Stats(); st.Lost != 1 || st.Written != 1 {
		t.Errorf("stats %+v", st)
	}
}

// TestRestartNeverAppends: a second incarnation with the same pid in the same hour (a container's pid 1)
// takes the next seq instead of appending to the file a torn last line may end.
func TestRestartNeverAppends(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.Local)
	for range 2 {
		l, err := Open(Config{Dir: dir, Role: "server", PID: 1, Now: func() time.Time { return at }})
		if err != nil {
			t.Fatal(err)
		}
		l.Emit(lifecycle("x"))
		closeLog(t, l)
	}
	// A torn file from a crash, and a gzipped one from retention, both count.
	day := filepath.Join(dir, "server", "2026-11-06")
	os.WriteFile(filepath.Join(day, "events-09-1-007.jsonl.gz"), nil, 0o644)
	l, _ := Open(Config{Dir: dir, Role: "server", PID: 1, Now: func() time.Time { return at }})
	l.Emit(lifecycle("y"))
	closeLog(t, l)
	var names []string
	for name, recs := range readAll(t, dir) {
		if len(recs) != 1 {
			t.Errorf("%s holds %d records, want 1", name, len(recs))
		}
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{"server/2026-11-06/events-09-1-001.jsonl", "server/2026-11-06/events-09-1-002.jsonl",
		"server/2026-11-06/events-09-1-008.jsonl"}
	if !slices.Equal(names, want) {
		t.Errorf("files %v, want %v", names, want)
	}
}
