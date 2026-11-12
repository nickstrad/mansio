package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/client"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
)

// Soak support (control-plane plan CP-4b, RI-15): pacing, a skewed database mix, an acknowledgement log that
// `frontbench verify` checks against the databases' rows, and an event log (errors, retries, moves) that the
// soak analysis lines up with its fault windows.

// pacer spaces requests across all workers to rate per second (an open-loop arrival rate, capped by conc).
type pacer struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func newPacer(rate float64) *pacer {
	if rate <= 0 {
		return nil
	}
	return &pacer{interval: time.Duration(float64(time.Second) / rate)}
}

// wait blocks until this caller's slot; a nil pacer never waits.
func (p *pacer) wait(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	t := p.next
	if t.Before(now) {
		t = now
	}
	p.next = t.Add(p.interval)
	p.mu.Unlock()
	if d := t.Sub(now); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
}

// picker chooses a database index: uniform, or Zipf with exponent s > 1 (rank 0 hottest) so that a few
// databases stay warm and the tail goes cold, passivates and re-warms.
func picker(r *rand.Rand, n int, s float64) func() int {
	if s <= 1 || n < 2 {
		return func() int { return r.IntN(n) }
	}
	z := rand.NewZipf(r, s, 1, uint64(n-1))
	return func() int { return int(z.Uint64()) }
}

// Write outcomes in the acknowledgement log.
const (
	ackOK      = "ok"      // acknowledged: must be present exactly once
	ackFailed  = "failed"  // a definite failure: nothing applied, must be absent
	ackUnknown = "unknown" // may have applied: present at most once
)

// soakLog writes the acknowledgement log (db, request id, outcome per write; tab-separated) and the event
// log (JSON lines). Either may be off.
type soakLog struct {
	mu          sync.Mutex
	acks, evs   *bufio.Writer
	files       []*os.File
	ackOn, evOn bool
}

func openSoakLog(ackPath, evPath string) (*soakLog, error) {
	l := &soakLog{}
	open := func(path string) (*bufio.Writer, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, errs.Wrap(module, err, "cannot open %s", path)
		}
		l.files = append(l.files, f)
		return bufio.NewWriterSize(f, 64<<10), nil
	}
	var err error
	if ackPath != "" {
		if l.acks, err = open(ackPath); err != nil {
			return nil, err
		}
		l.ackOn = true
	}
	if evPath != "" {
		if l.evs, err = open(evPath); err != nil {
			l.Close()
			return nil, err
		}
		l.evOn = true
	}
	return l, nil
}

// write records one write's outcome.
func (l *soakLog) write(db, rid string, err error) {
	if !l.ackOn {
		return
	}
	out := ackOK
	if err != nil {
		out = ackFailed
		var ce *client.Error
		if !errors.As(err, &ce) || ce.MayHaveApplied {
			out = ackUnknown
		}
	}
	l.mu.Lock()
	fmt.Fprintf(l.acks, "%s\t%s\t%s\n", db, rid, out)
	l.mu.Unlock()
}

// event records one event; t is added.
func (l *soakLog) event(kind string, fields map[string]any) {
	if !l.evOn {
		return
	}
	fields["t"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["ev"] = kind
	b, _ := json.Marshal(fields)
	l.mu.Lock()
	l.evs.Write(append(b, '\n'))
	l.mu.Unlock()
}

// callError records a call that did not end ok.
func (l *soakLog) callError(db string, kind front.Kind, took time.Duration, err error) {
	if !l.evOn || err == nil {
		return
	}
	f := map[string]any{"db": db, "kind": string(kind), "code": client.CodeOf(err), "took_ms": took.Milliseconds()}
	var ce *client.Error
	if errors.As(err, &ce) {
		f["op"], f["may_have_applied"], f["msg"] = ce.Op, ce.MayHaveApplied, ce.Message
	}
	l.event("error", f)
}

// retry records a retry decision: an answer the client did not give up on.
func (l *soakLog) retry(e client.RetryEvent) {
	l.event("retry", map[string]any{"db": e.DB, "code": e.Code, "attempt": e.Attempt, "reroute": e.Reroute,
		"hint_ms": e.Hint.Milliseconds(), "delay_ms": e.Delay.Milliseconds()})
}

func (l *soakLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var first error
	for _, w := range []*bufio.Writer{l.acks, l.evs} {
		if w != nil {
			if err := w.Flush(); err != nil && first == nil {
				first = err
			}
		}
	}
	for _, f := range l.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// VerifyReport is what `frontbench verify` found: the acknowledgement logs against every row of fb.
type VerifyReport struct {
	DBs         int            `json:"dbs"`
	Rows        int            `json:"rows"`
	Writes      map[string]int `json:"writes"`       // logged writes by outcome
	Lost        []string       `json:"lost"`         // acknowledged, not present
	Duplicated  []string       `json:"duplicated"`   // present more than once (any outcome)
	FailedFound []string       `json:"failed_found"` // reported as a definite failure, yet applied
	Unlogged    int            `json:"unlogged"`     // rows whose v is no logged request id (setup, other runs)
	UnknownSeen int            `json:"unknown_seen"` // may-have-applied writes that did apply
	OK          bool           `json:"ok"`
}

// verifyCmd reads every row of fb in each database (through the client, so through the edge if given) and
// checks the acknowledgement logs: no acknowledged write lost, none applied twice, no definite failure applied.
func verifyCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f locatorFlags
	var logs, out string
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.StringVar(&f.locator, "locator", "http://127.0.0.1:7080", "controld base URL")
	fs.StringVar(&f.tenant, "tenant", "fb", "tenant of the databases fb-0..fb-N-1")
	fs.StringVar(&f.scheme, "scheme", "http", "scheme for agent endpoints given as host:port")
	fs.StringVar(&f.ca, "ca", "", "PEM CA to trust for https")
	fs.StringVar(&f.edge, "edge", "", "send queries to this edge base URL")
	fs.IntVar(&f.dbs, "dbs", 16, "databases")
	fs.StringVar(&logs, "ack-logs", "", "comma-separated acknowledgement logs (frontbench locator -ack-log)")
	fs.StringVar(&out, "out", "", "write the report JSON here")
	if err := parse(fs, args, stderr); err != nil {
		return err
	}
	apiKey := os.Getenv("MANSIO_API_KEY")
	if logs == "" || apiKey == "" || f.dbs < 1 {
		fmt.Fprintln(stderr, "frontbench verify: need -ack-logs, -dbs >= 1 and MANSIO_API_KEY")
		return usageError{errors.New("bad flags")}
	}
	type logged struct{ db, outcome string }
	want := map[string]logged{}
	rep := VerifyReport{DBs: f.dbs, Writes: map[string]int{}}
	for _, path := range strings.Split(logs, ",") {
		b, err := os.ReadFile(path)
		if err != nil {
			return errs.Wrap(module, err, "cannot read %s", path)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			p := strings.Split(line, "\t")
			if len(p) != 3 {
				continue
			}
			want[p[1]] = logged{p[0], p[2]}
			rep.Writes[p[2]]++
		}
	}
	cfg := f.clientConfig(apiKey)
	cfg.Timeout, cfg.MaxReroutes = time.Minute, 20
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	seen := map[string]int{}
	const page = 2000
	for i := range f.dbs {
		for last := int64(0); ; {
			res, err := c.Do(ctx, client.Request{DB: fbName(i), Kind: front.KindRead,
				SQL: "select id, v from fb where id > ? order by id limit ?", Args: []any{last, int64(page)}})
			if err != nil {
				return errs.Wrap(module, err, "cannot read %s", fbName(i))
			}
			for _, row := range res.Rows {
				if id, ok := row[0].(json.Number); ok {
					last, _ = id.Int64()
				} else if id, ok := row[0].(float64); ok {
					last = int64(id)
				}
				v, _ := row[1].(string)
				seen[fbName(i)+"\t"+v]++
				rep.Rows++
			}
			if len(res.Rows) < page {
				break
			}
		}
	}
	for k, n := range seen {
		db, v, _ := strings.Cut(k, "\t")
		w, ok := want[v]
		switch {
		case !ok || w.db != db:
			rep.Unlogged += n
			continue
		case n > 1:
			rep.Duplicated = append(rep.Duplicated, v)
		}
		switch w.outcome {
		case ackFailed:
			rep.FailedFound = append(rep.FailedFound, v)
		case ackUnknown:
			rep.UnknownSeen++
		}
	}
	for rid, w := range want {
		if w.outcome == ackOK && seen[w.db+"\t"+rid] == 0 {
			rep.Lost = append(rep.Lost, rid)
		}
	}
	for _, s := range [][]string{rep.Lost, rep.Duplicated, rep.FailedFound} {
		sort.Strings(s)
	}
	rep.OK = len(rep.Lost) == 0 && len(rep.Duplicated) == 0 && len(rep.FailedFound) == 0
	fmt.Fprintf(stdout, "verify: %d rows in %d databases; writes %v; lost %d, duplicated %d, failed-but-applied %d, unknown applied %d, unlogged rows %d\n",
		rep.Rows, rep.DBs, rep.Writes, len(rep.Lost), len(rep.Duplicated), len(rep.FailedFound), rep.UnknownSeen, rep.Unlogged)
	if out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			return errs.Wrap(module, err, "cannot write %s", out)
		}
	}
	if !rep.OK {
		return errs.Wrap(module, nil, "acknowledged writes lost or duplicated (see the report)")
	}
	return nil
}
