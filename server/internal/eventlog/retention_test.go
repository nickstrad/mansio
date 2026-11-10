package eventlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPlanRetention(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	f := func(name string, size int64, age time.Duration) logFile { return logFile{name, size, now.Add(-age)} }
	files := []logFile{f("d", 10, 1*day), f("a", 10, 40*day), f("c", 10, 20*day), f("b", 10, 30*day), f("open", 10, 0), f("fresh", 10, time.Minute)}
	for _, tc := range []struct {
		name     string
		maxBytes int64
		maxAge   time.Duration
		want     []string
	}{
		{"within both limits", 60, 45 * day, nil},
		{"size on point: exactly the limit", 60, -1, nil},
		{"size off point: one byte over deletes the oldest", 59, -1, []string{"a"}},
		{"size: oldest first until it fits", 35, -1, []string{"a", "b", "c"}},
		{"age on point: exactly max age stays", -1, 40 * day, nil},
		{"age off point", -1, 40*day - time.Second, []string{"a"}},
		{"age and size together", 45, 25 * day, []string{"a", "b"}},
		{"the open file and files written in the last minutes are never deleted, though over the limit", 5, time.Second, []string{"a", "b", "c", "d"}},
		{"no limits", -1, -1, nil},
	} {
		var got []string
		for _, d := range planRetention(files, now, tc.maxBytes, tc.maxAge, retentionKeep, "open") {
			got = append(got, d.path)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: delete %v, want %v", tc.name, got, tc.want)
		}
	}
}

// B9's scenario: a writer that nobody ingests from. Before, the hourly and 64 MiB files piled up until the
// volume was full. With the limits, each new file is followed by a sweep: the tree stays under
// max_total_bytes plus what the last minutes wrote, old days go by age, files that are not the log's are left
// alone, and what was deleted is counted.
func TestRetentionBoundsTheTree(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	now := func() time.Time { return clock }
	// Leftovers from earlier runs: three old days, and two files retention must not touch.
	old := func(day string, age time.Duration, size int) string {
		p := filepath.Join(dir, "server", day, "events-07-1-001.jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, size), 0o644)
		os.Chtimes(p, clock.Add(-age), clock.Add(-age))
		return p
	}
	ancient := old("2026-09-01", 70*24*time.Hour, 100)
	lastWeek := old("2026-11-03", 7*24*time.Hour, 4000)
	yesterday := old("2026-11-09", 26*time.Hour, 4000)
	notes := filepath.Join(dir, "server", "2026-11-03", "notes.txt")
	os.WriteFile(notes, []byte("an operator's file"), 0o644)
	otherRole := filepath.Join(dir, "cli", "2026-09-01", "events-07-1-001.jsonl")
	os.MkdirAll(filepath.Dir(otherRole), 0o755)
	os.WriteFile(otherRole, make([]byte, 9000), 0o644)
	os.Chtimes(otherRole, clock.Add(-70*24*time.Hour), clock.Add(-70*24*time.Hour))

	l, err := Open(Config{Dir: dir, Role: "server", PID: 7, Now: now, MaxFileBytes: 1500, MaxTotalBytes: 10_000, MaxAge: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	var sent int64
	emit := func(n int) {
		for range n {
			l.Emit(Event{Level: Info, TraceID: "t", Payload: Lifecycle{Phase: "startup", Detail: fmt.Sprintf("%0200d", 0)}})
		}
		for sent += int64(n); l.Stats().Written < sent; { // written: flushed, so any roll and sweep are done
			time.Sleep(time.Millisecond)
		}
	}
	emit(1) // the first file opens: the sweep deletes by age
	if exists(ancient) || exists(filepath.Dir(ancient)) || !exists(lastWeek) || !exists(yesterday) {
		t.Fatalf("after the first sweep: ancient %v (its day %v), last week %v, yesterday %v; want only the 70-day-old file and its directory gone",
			exists(ancient), exists(filepath.Dir(ancient)), exists(lastWeek), exists(yesterday))
	}
	// An hour of writing 30 KB in 1.5 KB files, 12 minutes apart on the clock (so files leave the keep window).
	for range 20 {
		emit(5)
		clock = clock.Add(12 * time.Minute)
		os.Chtimes(l.path, clock, clock) // the fake clock does not move file times
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := l.Stats()
	var tree int64
	filepath.Walk(filepath.Join(dir, "server"), func(p string, info os.FileInfo, _ error) error {
		if info != nil && info.Mode().IsRegular() && eventFile.MatchString(info.Name()) {
			tree += info.Size()
		}
		return nil
	})
	// The ceiling: the limit, plus the file open at the last sweep, plus one being written.
	if tree > 10_000+2*1500+500 {
		t.Fatalf("the tree holds %d bytes of event files, over the 10 000 limit and two files' slack (written %d records)", tree, st.Written)
	}
	if exists(lastWeek) || exists(yesterday) {
		t.Fatalf("the oldest files outlived the size limit: last week %v, yesterday %v", exists(lastWeek), exists(yesterday))
	}
	if !exists(notes) || !exists(otherRole) {
		t.Fatalf("retention touched a file that is not this role's event file: notes %v, other role %v", exists(notes), exists(otherRole))
	}
	if st.Retired < 3 || st.RetiredBytes < 8100 || st.TreeBytes <= 0 || st.TreeBytes > tree+3000 {
		t.Fatalf("stats %+v: want at least the 3 old files (8 100 bytes) retired and the tree size reported (tree now %d)", st, tree)
	}
	t.Logf("wrote %d records in 1.5 KB files; retention deleted %d files (%d bytes); %d bytes left under a 10 000 limit", st.Written, st.Retired, st.RetiredBytes, tree)
}

// Records still queued when Close gives up are counted as lost, not silently dropped.
func TestCloseCountsWhatItAbandons(t *testing.T) {
	stall := make(chan struct{})
	l, err := open(Config{Dir: t.TempDir(), Role: "server", Buffer: 64}, func() { <-stall })
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		l.Emit(Event{Level: Info, TraceID: "t", Payload: Lifecycle{Phase: "startup"}})
	}
	for l.Stats().Depth != 9 { // the writer took one and is stuck in its write
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = l.Close(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "9 queued records are lost") {
		t.Fatalf("Close past its deadline: %v, want the deadline and the 9 queued records named", err)
	}
	close(stall) // the disk comes back: the writer finishes the record it held and counts the rest
	<-l.done
	if st := l.Stats(); st.Lost != 9 || st.Written != 1 || st.Depth != 0 {
		t.Fatalf("%+v: want 9 lost (counted), 1 written", st)
	}
}
