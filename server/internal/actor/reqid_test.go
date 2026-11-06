package actor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/det/dettest"
)

// CP-2 / PG-09: request ids. Spec tests for the pure parts, fault tests for the statement order, and
// integration tests on real SQLite for the gate: the same id retried after a crash and after a move applies
// exactly once, and a failed write leaves no id row.

func TestOutcomeEncodingRoundTrips(t *testing.T) {
	for _, rows := range []int64{0, 1, 63, 64, -1, 1 << 40} {
		got, err := decodeRecord(encodeOutcome(Result{Rows: rows}))
		if err != nil || got.closure || got.rows != rows {
			t.Fatalf("rows %d: got %+v, %v", rows, got, err)
		}
	}
	for _, body := range []string{"", "[]", `[{"_version":2},null,"id-1"]`} {
		got, err := decodeRecord(encodeBody([]byte(body)))
		if err != nil || !got.closure || got.body == nil || string(got.body) != body {
			t.Fatalf("body %q: got %+v, %v", body, got, err)
		}
	}
	// Off points: empty, a lone v1 byte, an unknown version, a truncated or overlong varint.
	for _, b := range [][]byte{nil, {}, {outcomeV1}, {3, 0}, {0, 0}, {outcomeV1, 0x80}, {outcomeV1, 0, 0}} {
		if _, err := decodeRecord(b); !errors.Is(err, errBadPriorResult) {
			t.Fatalf("%v: err %v, want errBadPriorResult", b, err)
		}
	}
}

func TestSweepDue(t *testing.T) {
	t0 := time.Unix(1000, 0)
	for _, tc := range []struct {
		name        string
		ready, more bool
		since       time.Duration
		want        bool
	}{
		{"table not known yet", false, true, time.Hour, false},
		{"just before the interval (off)", true, false, time.Minute - time.Millisecond, false},
		{"at the interval (on)", true, false, time.Minute, true},
		{"cut short last time", true, true, 0, true},
	} {
		if got := sweepDue(tc.ready, tc.more, t0, t0.Add(tc.since), time.Minute); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The claim runs before the statement and the result is recorded after it, all inside the write's savepoint;
// the table is created once, after BEGIN.
func TestRequestIDStatementOrder(t *testing.T) {
	f := &faultExec{}
	r := write("w1")
	r.RequestID = "id-1"
	submitAll(t, fakeActor(t, Config{}, f, nil), r, write("w2"))
	want := []string{"BEGIN IMMEDIATE", RequestTableDDL[0], RequestTableDDL[1],
		"SAVEPOINT w", sqlClaimRequest, "w1", sqlRecordResult, "RELEASE w",
		"SAVEPOINT w", "w2", "RELEASE w", "COMMIT"}
	if got := f.statements(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("statements:\n got %q\nwant %q", got, want)
	}
}

func TestRequestTableCreationFailureFailsTheBatch(t *testing.T) {
	boom := errors.New("disk full")
	f := &faultExec{}
	a := fakeActor(t, Config{}, f, nil)
	a.open = func(string, Config, bool) (executor, error) { return &failDDL{f, boom}, nil }
	r := write("w1")
	r.RequestID = "id-1"
	res := submitAll(t, a, r, write("w2"))
	if !errors.Is(res[0].Err, boom) || !errors.Is(res[1].Err, boom) || a.Commits() != 0 {
		t.Fatalf("results %v / %v, commits %d", res[0].Err, res[1].Err, a.Commits())
	}
}

type failDDL struct {
	*faultExec
	err error
}

func (f *failDDL) run(ctx context.Context, q string) error {
	if q == RequestTableDDL[0] {
		return f.err
	}
	return f.faultExec.run(ctx, q)
}

// doID runs one write with a request id.
func doID(t testing.TB, a *Actor, id, sql string, args ...any) Result {
	t.Helper()
	r := &Request{SQL: sql, Args: args, Write: true, RequestID: id}
	r.boundBy(ctx(t), time.Now())
	if err := a.Submit(r); err != nil {
		t.Fatal(err)
	}
	return r.Wait(ctx(t))
}

func readInt(t testing.TB, a *Actor, sql string, args ...any) int64 {
	t.Helper()
	r := a.Do(ctx(t), sql, false, args...)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	return r.First
}

const bump = `UPDATE kv SET v = v + 1 WHERE id = 1`

func idRows(t testing.TB, a *Actor) int64 {
	t.Helper()
	return readInt(t, a, `SELECT count(*) FROM _mansio_requests`)
}

func TestRequestIDAppliesOnce(t *testing.T) {
	a := startedKV(t, Config{}, 3)
	first := doID(t, a, "r-1", `UPDATE kv SET v = v + 1 WHERE id <= 2`)
	if first.Err != nil || first.Duplicate || first.Rows != 2 {
		t.Fatalf("first: %+v", first)
	}
	again := doID(t, a, "r-1", `UPDATE kv SET v = v + 1 WHERE id <= 2`)
	if again.Err != nil || !again.Duplicate || again.Rows != 2 {
		t.Fatalf("retry: %+v (want the original's 2 rows, duplicate)", again)
	}
	if v := readInt(t, a, `SELECT v FROM kv WHERE id = 1`); v != 11 {
		t.Fatalf("v = %d, want 11 (applied once)", v)
	}
	// A duplicate never runs its statement: one that would fail now is still answered ok.
	if r := doID(t, a, "ins", `INSERT INTO kv (id, v) VALUES (100, 0)`); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := doID(t, a, "ins", `INSERT INTO kv (id, v) VALUES (100, 0)`); r.Err != nil || !r.Duplicate || r.Rows != 1 {
		t.Fatalf("duplicate insert: %+v", r)
	}
	if r := doID(t, a, "r-2", bump); r.Err != nil || r.Duplicate {
		t.Fatalf("another id: %+v", r)
	}
	// Reads ignore ids; writes without one are never deduplicated.
	if r := a.Do(ctx(t), bump, true); r.Err != nil || r.Duplicate {
		t.Fatalf("no id: %+v", r)
	}
	if n := idRows(t, a); n != 3 {
		t.Fatalf("%d id rows, want 3", n)
	}
}

// The same id twice in one batch: the second sees the first's claim (same transaction) and does not run.
func TestRequestIDDuplicateInOneBatch(t *testing.T) {
	a := newKV(t, Config{}, 1)
	reqs := []*Request{
		{SQL: bump, Write: true, RequestID: "x"},
		{SQL: bump, Write: true, RequestID: "x"},
		{SQL: bump, Write: true, RequestID: "y"},
	}
	res := submitAll(t, a, reqs...)
	if res[0].Duplicate || !res[1].Duplicate || res[2].Duplicate || res[0].Batch != 3 {
		t.Fatalf("results %+v", res)
	}
	if v := readInt(t, a, `SELECT v FROM kv WHERE id = 1`); v != 12 {
		t.Fatalf("v = %d, want 12", v)
	}
}

// Gate: a failed user statement leaves no id row, whether it fails on its own, ends the transaction, times
// out, or is rolled back by a batch-mate; a retry with the same id then applies.
func TestFailedWriteLeavesNoRequestID(t *testing.T) {
	t.Run("constraint failure", func(t *testing.T) {
		a := startedKV(t, Config{}, 1)
		if r := doID(t, a, "f", `INSERT INTO kv (id, v) VALUES (1, 0)`); r.Err == nil {
			t.Fatal("duplicate key insert succeeded")
		}
		if r := doID(t, a, "ok", bump); r.Err != nil { // creates the table if the failure rolled it back
			t.Fatal(r.Err)
		}
		if n := readInt(t, a, `SELECT count(*) FROM _mansio_requests WHERE request_id = 'f'`); n != 0 {
			t.Fatalf("failed write left %d id rows", n)
		}
		if r := doID(t, a, "f", `INSERT INTO kv (id, v) VALUES (2, 0)`); r.Err != nil || r.Duplicate {
			t.Fatalf("retry of a failed id: %+v", r)
		}
	})
	t.Run("ends the transaction, with a batch-mate", func(t *testing.T) {
		a := newKV(t, Config{}, 1)
		res := submitAll(t, a,
			&Request{SQL: bump, Write: true, RequestID: "mate"},
			&Request{SQL: `INSERT OR ROLLBACK INTO kv (id, v) VALUES (1, 0)`, Write: true, RequestID: "ends"})
		if !errors.Is(res[0].Err, ErrAborted) || res[1].Err == nil {
			t.Fatalf("results %v / %v", res[0].Err, res[1].Err)
		}
		if r := doID(t, a, "mate", bump); r.Err != nil || r.Duplicate {
			t.Fatalf("retry of the aborted batch-mate: %+v", r)
		}
		if n := readInt(t, a, `SELECT count(*) FROM _mansio_requests WHERE request_id = 'ends'`); n != 0 {
			t.Fatalf("rolled-back write left %d id rows", n)
		}
	})
	t.Run("statement timeout", func(t *testing.T) {
		a := startedKV(t, Config{}, 1)
		r := &Request{SQL: `UPDATE kv SET v = (` + slowCTE + `)`, Args: []any{int64(1e9)}, Write: true, RequestID: "slow",
			StmtTimeout: 20 * time.Millisecond}
		if err := a.Submit(r); err != nil {
			t.Fatal(err)
		}
		if res := r.Wait(ctx(t)); !res.TimedOut {
			t.Fatalf("%+v", res)
		}
		if r := doID(t, a, "slow", bump); r.Err != nil || r.Duplicate {
			t.Fatalf("retry after a timeout: %+v", r)
		}
	})
}

// Gate: an applied write survives a crash with its id, so the retry after restart does not apply again. The
// child process applies the write and exits without closing anything (no checkpoint, the WAL left behind).
func TestRequestIDAfterCrash(t *testing.T) {
	if path := os.Getenv("MANSIO_ACTOR_CRASH_DB"); path != "" {
		a := New(0, path, Config{})
		if err := a.Start(true); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if r := doID(t, a, "crash-1", bump); r.Err != nil || r.Duplicate {
			fmt.Fprintln(os.Stderr, r.Err)
			os.Exit(2)
		}
		os.Exit(7) // acknowledged, then gone: the client never saw the answer and retries
	}
	path := seededKV(t, Config{}, 1)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRequestIDAfterCrash$")
	cmd.Env = append(os.Environ(), "MANSIO_ACTOR_CRASH_DB="+path)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("expected the crashed child's WAL to be left behind: %v", err)
	}
	a := New(0, path, Config{})
	t.Cleanup(a.Close)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	if r := doID(t, a, "crash-1", bump); r.Err != nil || !r.Duplicate || r.Rows != 1 {
		t.Fatalf("retry after crash: %+v", r)
	}
	if v := readInt(t, a, `SELECT v FROM kv WHERE id = 1`); v != 11 {
		t.Fatalf("v = %d, want 11 (applied exactly once)", v)
	}
}

// Gate: a move copies the database (snapshot → new owner). The id travels with it: the retry at the new owner
// is a duplicate, and a write the old owner applied after the snapshot is not in the copy, so it applies there.
func TestRequestIDAfterMove(t *testing.T) {
	old := startedKV(t, Config{ReadConns: 2}, 1)
	if r := doID(t, old, "before", bump); r.Err != nil {
		t.Fatal(r.Err)
	}
	copyPath := filepath.Join(t.TempDir(), "moved.db")
	if _, err := old.Snapshot(ctx(t), copyPath); err != nil {
		t.Fatal(err)
	}
	if r := doID(t, old, "after", bump); r.Err != nil { // the old owner applied it, but past the copy
		t.Fatal(r.Err)
	}
	old.Close()

	moved := New(1, copyPath, Config{ReadConns: 2})
	t.Cleanup(moved.Close)
	if err := moved.Start(true); err != nil {
		t.Fatal(err)
	}
	if r := doID(t, moved, "before", bump); r.Err != nil || !r.Duplicate || r.Rows != 1 {
		t.Fatalf("retry at the new owner: %+v", r)
	}
	if r := doID(t, moved, "after", bump); r.Err != nil || r.Duplicate {
		t.Fatalf("write not in the copy: %+v", r)
	}
	if v := readInt(t, moved, `SELECT v FROM kv WHERE id = 1`); v != 12 {
		t.Fatalf("v = %d, want 12 (each id applied once in this copy)", v)
	}
}

// Retention: ids older than RequestTTL are deleted in bounded, set-based sweeps on write batches. After that, a
// retry of an expired id applies again: the TTL must outlast every client retry window.
func TestRequestIDRetention(t *testing.T) {
	clk := dettest.NewClock(time.Unix(1_000_000, 0))
	a := newKV(t, Config{RequestTTL: time.Hour, RequestSweep: time.Minute, Clock: clk}, 1)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	if r := doID(t, a, "old", bump); r.Err != nil {
		t.Fatal(r.Err)
	}
	// 2500 more expired ids: three sweeps of at most sweepBatch.
	if r := a.Do(ctx(t), `INSERT INTO _mansio_requests (request_id, applied_ms)
  WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2500) SELECT 'bulk-' || i, 0 FROM n`, true); r.Err != nil {
		t.Fatal(r.Err)
	}
	clk.Advance(time.Hour + time.Minute)
	want := []int64{2501 + 1 - sweepBatch, 2501 + 2 - 2*sweepBatch, 3} // each write adds its own id
	for i, w := range want {
		if r := doID(t, a, fmt.Sprintf("new-%d", i), `UPDATE kv SET v = v WHERE id = 1`); r.Err != nil {
			t.Fatal(r.Err)
		}
		if n := idRows(t, a); n != w {
			t.Fatalf("after sweep %d: %d id rows, want %d", i, n, w)
		}
	}
	if r := doID(t, a, "old", bump); r.Err != nil || r.Duplicate {
		t.Fatalf("expired id: %+v (want applied again)", r)
	}
	// Within the TTL nothing is swept: the young ids stay.
	clk.Advance(2 * time.Minute)
	if r := doID(t, a, "young", bump); r.Err != nil {
		t.Fatal(r.Err)
	}
	if n := idRows(t, a); n != 5 {
		t.Fatalf("%d id rows, want 5", n)
	}
}

// Every agent table carries ReservedPrefix, so front's guard covers it.
func TestRequestTableUsesTheReservedPrefix(t *testing.T) {
	for _, q := range append(RequestTableDDL, sqlClaimRequest, sqlRecordResult, sqlPriorResult, sqlSweepRequests) {
		if !strings.Contains(q, ReservedPrefix+"requests") {
			t.Fatalf("%q does not name %srequests", q, ReservedPrefix)
		}
	}
}
