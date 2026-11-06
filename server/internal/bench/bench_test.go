package bench

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/outcome"
	"github.com/nickstrad/mansio/server/internal/panicx"
	"github.com/nickstrad/mansio/server/internal/schema"
	"github.com/nickstrad/mansio/server/internal/stats"
	"github.com/nickstrad/mansio/server/internal/workload"
)

// --- specification tests (pure) --------------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	other := errors.New("no such table")
	tests := []struct {
		name string
		res  actor.Result
		want outcome.Code
	}{
		{"success", actor.Result{}, outcome.OK},
		{"statement timeout wins over its error", actor.Result{Err: other, TimedOut: true}, outcome.StmtTimeout},
		{"mailbox full", actor.Result{Err: actor.ErrMailboxFull}, outcome.Shed},
		{"expired in queue", actor.Result{Err: actor.ErrExpired}, outcome.Expired},
		{"batch aborted", actor.Result{Err: actor.ErrAborted}, outcome.Aborted},
		{"actor closed", actor.Result{Err: actor.ErrClosed}, outcome.Closed},
		{"client stopped waiting", actor.Result{Err: context.DeadlineExceeded}, outcome.ClientTimeout},
		{"wrapped sentinel still matches", actor.Result{Err: fmt.Errorf("x: %w", actor.ErrMailboxFull)}, outcome.Shed},
		{"anything else", actor.Result{Err: other}, outcome.Error},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.res); got != tc.want {
				t.Fatalf("Classify = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRecords(t *testing.T) {
	start := time.Unix(100, 0)
	at := func(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }
	pk := load.Pick{DB: 7, Client: 2, Record: true, Statement: workload.Statement{Class: workload.Insert, Write: true}}
	j := newJob(3, pk, at(10))
	j = retryOf(j, 5*time.Millisecond)
	j.sched = at(20)

	r := answered(j, actor.Result{Rows: 1, Batch: 1 << 20, QueueWait: time.Millisecond}, at(21), at(25), start)
	if r.Phase != 3 || r.Attempt != 2 || r.DB != 7 || r.Client != 2 || int(r.Class) != workload.Index(workload.Insert) || !r.Write {
		t.Fatalf("identity fields: %+v", r)
	}
	if r.SchedNs != 20e6 || r.SendNs != 21e6 || r.LatencyNs != 4e6 || r.E2ENs != 15e6 || r.QueueNs != 1e6 {
		t.Fatalf("times: %+v", r)
	}
	if r.Batch != 1<<15-1 || r.Outcome != outcome.OK {
		t.Fatalf("batch must saturate at int16, outcome ok: %+v", r)
	}
	if j.prevDelay != 5*time.Millisecond {
		t.Fatalf("retryOf did not carry the delay")
	}

	n := notSent(j, start)
	if n.Outcome != outcome.GenFull || !n.Final || n.SendNs != n.SchedNs || n.LatencyNs != 0 {
		t.Fatalf("notSent: %+v", n)
	}
}

func TestResolveAndDBPath(t *testing.T) {
	c := tinyConfig(t, "")
	c.OutDir, c.DBDir = "", ""
	Resolve(c, time.Date(2026, 11, 4, 20, 1, 2, 0, time.UTC), "/tmp")
	if c.OutDir != filepath.Join("results", "tiny-20261104-200102") {
		t.Fatalf("OutDir = %s", c.OutDir)
	}
	if !strings.HasPrefix(c.DBDir, "/tmp/mansio-server/v3-") {
		t.Fatalf("DBDir = %s", c.DBDir)
	}
	keep := tinyConfig(t, "")
	keep.OutDir, keep.DBDir = "o", "d"
	Resolve(keep, time.Now(), "/tmp")
	if keep.OutDir != "o" || keep.DBDir != "d" {
		t.Fatal("Resolve must not override explicit directories")
	}
	if DBPath("d", 42) != filepath.Join("d", "db-000042.sqlite") {
		t.Fatal(DBPath("d", 42))
	}
}

func TestPlanWarnsAboutDescriptors(t *testing.T) {
	c := tinyConfig(t, "")
	need := uint64(c.MaxDBs() * 3)
	has := func(lines []string) bool { return strings.Contains(strings.Join(lines, "\n"), "descriptors") }
	if has(Plan(c, need)) {
		t.Fatal("warned at exactly the limit (off point)")
	}
	if !has(Plan(c, need-1)) {
		t.Fatal("no warning one below the limit (on point)")
	}
}

func TestSchemaRows(t *testing.T) {
	s := &schema.Schema{DB: 4, Tables: []schema.Table{
		{Name: "a", Parent: -1, SeedRows: 3, Columns: []schema.Column{{Name: "x", Type: schema.Text, Indexed: true}, {Name: "y", Type: schema.Blob}}},
		{Name: "b", Parent: 0, SeedRows: 1, Columns: []schema.Column{{Name: "z", Type: schema.Real}}},
	}}
	rows := schemaRows(s)
	if len(rows) != 2 || len(rows[0]) != len(schemaColumns) {
		t.Fatalf("%v", rows)
	}
	if fmt.Sprint(rows[0]) != "[4 a 2 1 false 3 TEXT BLOB]" || fmt.Sprint(rows[1]) != "[4 b 1 0 true 1 REAL]" {
		t.Fatalf("%v", rows)
	}
}

// --- integration -----------------------------------------------------------------------------------------------

// tinyConfig is a sub-second run on a few small databases; extra is spliced into the top-level JSON.
func tinyConfig(t testing.TB, extra string) *load.Config {
	t.Helper()
	body := `{"name":"tiny","workers":16,"keep_dbs":false,"sample_every":"50ms",` + extra + `
		"schema":{"min_tables":1,"max_tables":2,"min_rows":5,"max_rows":20},
		"clients":[{"name":"good","share":1,"retry":{"max_attempts":3,"backoff":"fixed","base":"1ms"}},
		           {"name":"bad","share":1,"retry":{"max_attempts":4,"retry_on":["any"]}}],
		"phases":[{"name":"one","dbs":2,"duration":"200ms","start_qps":300},
		          {"name":"two","dbs":4,"duration":"200ms","start_qps":300,"end_qps":100}]}`
	c, err := load.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c.OutDir, c.DBDir = filepath.Join(dir, "out"), filepath.Join(dir, "dbs")
	if err := os.MkdirAll(c.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return c
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rows
}

func TestRunWritesEveryFile(t *testing.T) {
	c := tinyConfig(t, "")
	sum, err := Run(context.Background(), c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Attempts == 0 || sum.Written != sum.Attempts || sum.Dropped != 0 {
		t.Fatalf("summary: %+v", sum)
	}
	reqs := readCSV(t, filepath.Join(c.OutDir, "requests.csv"))
	if int64(len(reqs)-1) != sum.Written {
		t.Fatalf("%d rows for %d records", len(reqs)-1, sum.Written)
	}
	finals := 0
	for _, row := range reqs[1:] {
		if _, ok := outcome.Parse(row[8]); !ok {
			t.Fatalf("bad outcome %q", row[8])
		}
		if row[3] == "true" {
			finals++
		}
	}
	firsts := int64(0)
	for _, cl := range sum.Clients {
		firsts += cl.Firsts
	}
	// Every request ends in exactly one final attempt (nothing was lost: gen_full rows are final too).
	if int64(finals) != firsts {
		t.Fatalf("%d final attempts for %d requests", finals, firsts)
	}
	if phases := readCSV(t, filepath.Join(c.OutDir, "phases.csv")); len(phases) != 3 || phases[2][3] != "2" {
		t.Fatalf("phases.csv: %v", phases)
	}
	if samples := readCSV(t, filepath.Join(c.OutDir, "samples.csv")); len(samples) < 3 {
		t.Fatalf("only %d sample rows", len(samples))
	}
	if _, err := os.Stat(filepath.Join(c.OutDir, "run.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.DBDir); !os.IsNotExist(err) {
		t.Fatal("keep_dbs=false must remove the database dir")
	}
}

func TestKeptDatabasesAreReused(t *testing.T) {
	c := tinyConfig(t, `"keep_dbs":true,`)
	c.Phases = c.Phases[:1]
	c.Phases[0].Duration = load.Duration{Duration: 20 * time.Millisecond}
	if _, err := Run(context.Background(), c, quiet()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(DBPath(c.DBDir, 0))
	c2 := *c
	c2.OutDir = filepath.Join(t.TempDir(), "out2")
	os.MkdirAll(c2.OutDir, 0o755)
	if _, err := Run(context.Background(), &c2, quiet()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(DBPath(c.DBDir, 0))
	if before == nil || after == nil || !os.SameFile(before, after) {
		t.Fatal("a kept database was rebuilt instead of reused")
	}
}

func TestCancelStopsSendingButFinishesTheFiles(t *testing.T) {
	c := tinyConfig(t, "")
	c.Phases[1].Duration = load.Duration{Duration: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	t0 := time.Now()
	sum, err := Run(ctx, c, quiet())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if e, ok := errs.Outermost(err); !ok || e.Module != module {
		t.Fatalf("error is not well-formed by bench: %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("took %s to stop", d)
	}
	if rows := readCSV(t, filepath.Join(c.OutDir, "requests.csv")); int64(len(rows)-1) != sum.Written || sum.Written == 0 {
		t.Fatalf("%d rows, summary says %d", len(rows)-1, sum.Written)
	}
}

// A panic in a worker goroutine reaches the caller's goroutine as *Panic.
func TestCrashForwardsTheFirstPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := crash{cancel: cancel}
	done := make(chan struct{})
	for i := range 3 {
		go func() {
			defer func() { done <- struct{}{} }()
			defer c.guard()
			panic(fmt.Sprint("boom ", i))
		}()
	}
	for range 3 {
		<-done
	}
	if ctx.Err() == nil || !c.happened() {
		t.Fatal("a panic must cancel the run")
	}
	defer func() {
		p, ok := recover().(*panicx.Panic)
		if !ok || !strings.HasPrefix(fmt.Sprint(p.Value), "boom") || !strings.Contains(p.Stack, "TestCrashForwardsTheFirstPanic") {
			t.Fatalf("recovered %#v", p)
		}
	}()
	c.repanic()
}

func TestEnqueueRefusesWhenTheQueueIsFull(t *testing.T) {
	r := &runner{jobs: make(chan job)} // no buffer, no worker
	if r.enqueue(job{}) || r.outstanding.Load() != 0 {
		t.Fatalf("enqueue into a full queue: outstanding %d", r.outstanding.Load())
	}
}

// Overload: one worker, slow statements, a fast schedule and a retry budget. Requests that find the queue full
// are recorded as gen_full, retries are refused by the budget, and still every request ends in exactly one
// final attempt.
func TestOverloadRecordsEveryRequestOnce(t *testing.T) {
	c := tinyConfig(t, `"statement_timeout":"20ms","mix":{"slow_cte":1,"point_read":1},`)
	c.Workers = 1
	c.Clients = c.Clients[:1]
	c.Clients[0].Retry = load.RetryConfig{MaxAttempts: 5, RetryOn: []string{"stmt_timeout", "gen_full"}, BudgetRatio: 0.01}
	c.Phases = c.Phases[:1]
	c.Phases[0].StartQPS, c.Phases[0].EndQPS = 2000, 2000
	c.Phases[0].ClientShares = nil
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	reqs := readCSV(t, filepath.Join(c.OutDir, "requests.csv"))
	finals, genFull := 0, 0
	for _, row := range reqs[1:] {
		if row[3] == "true" {
			finals++
		}
		if row[8] == "gen_full" {
			genFull++
		}
	}
	if genFull == 0 {
		t.Fatal("one worker at 2000 qps of slow statements should overflow the queue")
	}
	firsts, retries := sum.Clients[0].Firsts, sum.Clients[0].Retries
	if int64(finals) != firsts {
		t.Fatalf("%d final attempts for %d requests", finals, firsts)
	}
	if limit := int64(float64(firsts)*0.01) + 10; retries > limit {
		t.Fatalf("%d retries exceed the budget %d", retries, limit)
	}
}

// Ctrl-C does not wait out long backoffs: pending retries are cancelled and recorded as never sent.
func TestCancelDoesNotWaitOutBackoffs(t *testing.T) {
	c := tinyConfig(t, `"statement_timeout":"5ms","mix":{"slow_cte":1},`)
	c.Clients = c.Clients[:1]
	c.Clients[0].Retry = load.RetryConfig{MaxAttempts: 3, Backoff: "fixed", Base: load.Duration{Duration: time.Hour},
		RetryOn: []string{"stmt_timeout"}}
	c.Phases = c.Phases[:1]
	c.Phases[0].Duration = load.Duration{Duration: time.Hour}
	c.Phases[0].ClientShares = nil
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	t0 := time.Now()
	sum, err := Run(ctx, c, quiet())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("took %s: waited out hour-long backoffs", d)
	}
	if sum.RetriesScheduled == 0 || sum.RetriesLost != sum.RetriesScheduled {
		t.Fatalf("scheduled %d, lost %d: every pending retry should be recorded as never sent", sum.RetriesScheduled, sum.RetriesLost)
	}
}

// A run that stops between a worker's "not stopping" check and its backoff's registration (the cancel sweep has
// already run) must not leave that backoff pending: the drain would wait it out. The hook stops the run inside that
// window, which under load happened by chance (TestCancelDoesNotWaitOutBackoffs once took 6m16s, a full-jitter
// draw of the hour-long delay).
func TestRetryDecidedAsTheRunStopsIsNotWaitedOut(t *testing.T) {
	c := tinyConfig(t, "")
	c.Clients[0].Retry = load.RetryConfig{MaxAttempts: 3, Backoff: "fixed", Base: load.Duration{Duration: time.Hour},
		RetryOn: []string{"stmt_timeout"}}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	r, err := newRunner(context.Background(), c, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for b := range r.backoffs {
			b.t.Stop()
		}
		r.requests.Close()
		r.closeFiles()
		r.cancel()
	})
	r.beforeBackoff = func() { // what run does when the dispatcher stops early
		r.stopping.Store(true)
		r.cancelBackoffs()
	}
	now := time.Now()
	j := newJob(0, load.Pick{Record: true, Statement: workload.Statement{Class: workload.Classes[0]}}, now)
	rng := rand.New(rand.NewPCG(1, 2))
	if !r.maybeRetry(j, stats.Record{Outcome: outcome.StmtTimeout}, 0, rng, now) {
		t.Fatal("retry not scheduled")
	}
	if n, pending := r.outstanding.Load(), len(r.backoffs); n != 0 || pending != 0 {
		t.Fatalf("outstanding %d, pending backoffs %d: the drain would wait out the hour-long backoff", n, pending)
	}
	if lost, rec := r.retriesLost.Load(), r.recorded.Load(); lost != 1 || rec != 1 {
		t.Fatalf("lost %d, recorded %d: the retry should be recorded once as never sent", lost, rec)
	}
}

// PG-05: a generated database enforces its foreign key and its column types. A violation is the statement's own
// error (outcome "error", not "aborted"), and its batch-mates still commit (relies on PG-01's savepoints).
func TestGeneratedDatabaseEnforcesForeignKeysAndTypes(t *testing.T) {
	s := &schema.Schema{Tables: []schema.Table{
		{Name: "a", Parent: -1, SeedRows: 2, Columns: []schema.Column{{Name: "x", Type: schema.Integer, Cardinality: 5}}},
		{Name: "b", Parent: 0, SeedRows: 2, Columns: []schema.Column{{Name: "y", Type: schema.Text, Cardinality: 5}}},
	}}
	path := filepath.Join(t.TempDir(), "db.sqlite")
	if err := actor.Seed(path, actor.Config{}, s.DDL(), func(exec func(string, ...any) error) error {
		return s.Fill(1, exec)
	}); err != nil {
		t.Fatal(err)
	}
	a := actor.New(0, path, actor.Config{})
	t.Cleanup(a.Close)
	tests := []struct {
		name string
		sql  string
		args []any
		want outcome.Code
	}{
		{"child of an existing parent (on point)", `INSERT INTO b (parent_id, y) VALUES (?, ?)`, []any{int64(2), "ok"}, outcome.OK},
		{"child of a missing parent", `INSERT INTO b (parent_id, y) VALUES (?, ?)`, []any{int64(3), "orphan"}, outcome.Error},
		{"child with no parent (NULL)", `INSERT INTO b (parent_id, y) VALUES (?, ?)`, []any{nil, "root"}, outcome.OK},
		{"text into an INTEGER column", `INSERT INTO a (x) VALUES (?)`, []any{"abc"}, outcome.Error},
		{"integer text into an INTEGER column (lossless, accepted)", `INSERT INTO a (x) VALUES (?)`, []any{"7"}, outcome.OK},
		{"delete a referenced parent", `DELETE FROM a WHERE id = ?`, []any{int64(1)}, outcome.Error},
	}
	reqs := make([]*actor.Request, len(tests))
	for i, tc := range tests { // one batch: queued before the actor starts
		reqs[i] = &actor.Request{SQL: tc.sql, Args: tc.args, Write: true}
		if err := a.Submit(reqs[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Start(false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, tc := range tests {
		if got := Classify(reqs[i].Wait(ctx)); got != tc.want {
			t.Errorf("%s: outcome %v, want %v", tc.name, got, tc.want)
		}
	}
	if r := a.Do(ctx, `SELECT COUNT(*) FROM b`, false); r.First != 4 {
		t.Fatalf("b has %d rows, want 2 seeded + 2 committed batch-mates", r.First)
	}
}
