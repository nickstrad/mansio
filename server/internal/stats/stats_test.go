package stats

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/metrics"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/outcome"
)

var testFormat = RecordFormat{Classes: []string{"point_read", "insert"}, Clients: []string{"good", "bad"}}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll() // also checks every row has the header's width
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// --- specification tests -------------------------------------------------------------------------------------

func TestAppendSpec(t *testing.T) {
	r := Record{Phase: 3, Client: 1, Attempt: 2, Final: true, DB: 42, Class: 1, Write: true, Outcome: outcome.StmtTimeout,
		Batch: 7, Rows: 1, SchedNs: 1_500_000, SendNs: 2_000_000, LatencyNs: 1_234_567, E2ENs: 2e6, QueueNs: 50, ExecNs: 1e6, CommitNs: 3e5}
	want := "3,bad,2,true,42,insert,true,false,stmt_timeout,false,7,1,1.500,2.000,1234.6,2000.0,0.1,1000.0,300.0\n"
	if got := string(testFormat.Append(nil, &r)); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if h := testFormat.Header(); strings.Count(h, ",") != strings.Count(want, ",") || !strings.HasSuffix(h, "\n") {
		t.Fatalf("header %q does not match the row width", h)
	}
}

func TestFormatValidate(t *testing.T) {
	for _, bad := range []string{"", "a,b", `say "hi"`, "two\nlines"} {
		if err := (RecordFormat{Clients: []string{bad}}).Validate(); err == nil {
			t.Errorf("client name %q accepted", bad)
		}
	}
	if err := testFormat.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestField(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{"x", "x"}, {1.5, "1.5"}, {3.0, "3"}, {int64(-7), "-7"}, {true, "true"},
		{1500 * time.Microsecond, "1.5"}, {uint64(9), "9"},
	}
	for _, tc := range tests {
		if got := Field(tc.in); got != tc.want {
			t.Errorf("Field(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDeltaQuantileUsesOnlyNewObservations(t *testing.T) {
	h := &metrics.Float64Histogram{Buckets: []float64{0, 1, 2, 3, 4}, Counts: []uint64{100, 0, 0, 0}}
	prev := append([]uint64(nil), h.Counts...)
	h.Counts = []uint64{100, 0, 0, 10} // ten new observations, all in [3, 4)
	if q := deltaQuantile(prev, h, 0.5); q != 4 {
		t.Fatalf("p50 of new observations = %v, want 4", q)
	}
	if q := deltaQuantile(prev, &metrics.Float64Histogram{Buckets: h.Buckets, Counts: prev}, 0.99); q != 0 {
		t.Fatalf("no new observations should give 0, got %v", q)
	}
	open := &metrics.Float64Histogram{Buckets: []float64{0, 1, math.Inf(1)}, Counts: []uint64{0, 5}}
	if q := deltaQuantile(nil, open, 0.99); q != 1 {
		t.Fatalf("open-ended bucket should report its lower bound, got %v", q)
	}
}

// --- integration ------------------------------------------------------------------------------------------------

func TestRequestWriterStreamsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.csv")
	w, err := NewRequestWriter(path, testFormat)
	if err != nil {
		t.Fatal(err)
	}
	buf := w.NewBuffer()
	for i := range 2500 {
		buf.Add(Record{Client: 1, DB: int32(i), Class: uint8(i % 2), Outcome: outcome.Shed})
	}
	buf.Flush()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	rows := readCSV(t, path)
	if len(rows) != 2501 || w.Written() != 2500 || w.Dropped() != 0 {
		t.Fatalf("%d rows (want header + 2500), written %d, dropped %d", len(rows), w.Written(), w.Dropped())
	}
	if rows[0][8] != "err" || rows[2][8] != "shed" || rows[2][5] != "insert" {
		t.Fatalf("row 2: %v", rows[2])
	}
}

func TestNewRequestWriterAssertsValidNames(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out (-tags noassert)")
	}
	defer func() {
		if p := recover(); p == nil || !strings.HasPrefix(fmt.Sprint(p), "assertion failed") {
			t.Fatalf("want an assertion failure, got %v", p)
		}
	}()
	NewRequestWriter(filepath.Join(t.TempDir(), "r.csv"), RecordFormat{Clients: []string{"a,b"}})
}

func TestCSVQuotesAwkwardFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phases.csv")
	c, err := NewCSV(path, "name", "mix")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Row(`burst, "big"`, "a=1 b=2"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if rows := readCSV(t, path); rows[1][0] != `burst, "big"` {
		t.Fatalf("round trip: %q", rows[1][0])
	}
}

func TestSamplerWritesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.csv")
	zero := func() int { return 0 }
	s, err := NewSampler(path, 10*time.Millisecond, time.Now(), Gauges{
		Phase: zero, DBs: zero, TargetQPS: func() float64 { return 0 },
		Actors: func() (int, int, int) { return 0, 0, 0 }, InFlight: func() int64 { return 0 },
		Completed: func() int64 { return 0 }, Dropped: func() int64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	time.Sleep(60 * time.Millisecond)
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	rows := readCSV(t, path)
	if len(rows) < 3 || len(rows[0]) != len(sampleColumns) {
		t.Fatalf("%d rows of width %d", len(rows), len(rows[0]))
	}
	if g, err := strconv.Atoi(rows[1][5]); err != nil || g < 1 {
		t.Fatalf("goroutines = %q", rows[1][5])
	}
}

// --- properties ---------------------------------------------------------------------------------------------------

// Property: every record becomes one CSV row that encoding/csv parses back to the same values.
func FuzzRecordFormatRoundTrip(f *testing.F) {
	f.Add(int16(0), uint8(0), uint8(1), true, int32(5), uint8(1), uint8(3), int64(1), int64(123456789))
	f.Add(int16(-1), uint8(1), uint8(255), false, int32(-9), uint8(0), uint8(99), int64(-5), int64(math.MaxInt64))
	f.Fuzz(func(t *testing.T, phase int16, client, attempt uint8, final bool, db int32, class, code uint8, rows, ns int64) {
		r := Record{Phase: phase, Client: client % 2, Attempt: attempt, Final: final, DB: db, Class: class % 2,
			Outcome: outcome.Code(code % uint8(len(outcome.Codes()))), Rows: int32(rows), LatencyNs: ns, SchedNs: ns}
		line := testFormat.Append(nil, &r)
		got, err := csv.NewReader(strings.NewReader(string(line))).ReadAll()
		if err != nil || len(got) != 1 || len(got[0]) != len(recordColumns) {
			t.Fatalf("%q: %v %v", line, got, err)
		}
		row := got[0]
		if row[0] != strconv.Itoa(int(phase)) || row[1] != testFormat.Clients[r.Client] || row[4] != strconv.Itoa(int(db)) ||
			row[5] != testFormat.Classes[r.Class] || row[8] != r.Outcome.String() || row[3] != strconv.FormatBool(final) {
			t.Fatalf("round trip mismatch: %v from %+v", row, r)
		}
		if back, ok := outcome.Parse(row[8]); !ok || back != r.Outcome {
			t.Fatalf("outcome %q does not parse back", row[8])
		}
		lat, err := strconv.ParseFloat(row[14], 64)
		if err != nil || math.Abs(lat-float64(ns)/1e3) > 0.05+math.Abs(float64(ns))*1e-12 {
			t.Fatalf("latency_us %q from %d ns", row[14], ns)
		}
	})
}

// When the writer falls behind, a handed-off buffer is dropped and counted, never blocking the producer.
func TestSubmitDropsWhenTheWriterIsBehind(t *testing.T) {
	w := &RequestWriter{ch: make(chan []Record)} // nobody reading
	w.submit(make([]Record, 3))
	if w.Dropped() != 3 || w.Written() != 0 {
		t.Fatalf("dropped %d written %d", w.Dropped(), w.Written())
	}
}
