package actor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// Specification tests for the pure batch decisions (no SQLite, no clock).

func TestExpiredAt(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		name     string
		deadline time.Time
		want     bool
	}{
		{"no deadline never expires", time.Time{}, false},
		{"deadline in the future", now.Add(time.Second), false},
		{"deadline exactly now is not expired (on point)", now, false},
		{"deadline 1ns ago is expired (off point)", now.Add(-time.Nanosecond), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&Request{Deadline: tc.deadline}).expiredAt(now); got != tc.want {
				t.Fatalf("expiredAt = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplit(t *testing.T) {
	now := time.Unix(1000, 0)
	past := now.Add(-time.Second)
	w1, w2 := &Request{SQL: "w1", Write: true}, &Request{SQL: "w2", Write: true}
	r1, r2 := &Request{SQL: "r1"}, &Request{SQL: "r2"}
	xw, xr := &Request{SQL: "xw", Write: true, Deadline: past}, &Request{SQL: "xr", Deadline: past}
	tests := []struct {
		name                   string
		batch                  []*Request
		expired, writes, reads []*Request
	}{
		{"empty batch", nil, nil, nil, nil},
		{"one read", []*Request{r1}, nil, nil, []*Request{r1}},
		{"one write", []*Request{w1}, nil, []*Request{w1}, nil},
		{"interleaved keeps arrival order within each part", []*Request{r1, w1, r2, w2}, nil, []*Request{w1, w2}, []*Request{r1, r2}},
		{"expired writes and reads never run", []*Request{xw, r1, xr, w1}, []*Request{xw, xr}, []*Request{w1}, []*Request{r1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, w, r := split(tc.batch, now)
			if !slices.Equal(e, tc.expired) || !slices.Equal(w, tc.writes) || !slices.Equal(r, tc.reads) {
				t.Fatalf("split = %v %v %v", names(e), names(w), names(r))
			}
		})
	}
}

func names(rs []*Request) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.SQL)
	}
	return out
}

func TestSettle(t *testing.T) {
	own := errors.New("constraint failed")
	commitErr := errors.New("disk full")
	tests := []struct {
		name    string
		in      []error // each write's own error before settling
		aborted bool
		endErr  error
		want    []error
	}{
		{"committed: everyone keeps their own outcome", []error{nil, own, nil}, false, nil, []error{nil, own, nil}},
		{"aborted: successes become aborted, own failures stay", []error{nil, own, nil}, true, nil, []error{ErrAborted, own, ErrAborted}},
		{"commit failed: successes get the commit error", []error{nil, own}, false, commitErr, []error{commitErr, own}},
		{"aborted wins over the rollback error", []error{nil}, true, commitErr, []error{ErrAborted}},
		{"no writes", nil, false, commitErr, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := make([]Result, len(tc.in))
			for i, e := range tc.in {
				results[i].Err = e
			}
			settle(results, tc.aborted, tc.endErr)
			for i, w := range tc.want {
				if !errors.Is(results[i].Err, w) || (w == nil) != (results[i].Err == nil) {
					t.Fatalf("write %d: err = %v, want %v", i, results[i].Err, w)
				}
			}
		})
	}
}

func TestIsWrite(t *testing.T) {
	tests := map[string]bool{
		"SELECT 1": false, "  select * from t": false, "VALUES (1)": false, "EXPLAIN SELECT 1": false,
		"WITH c AS (SELECT 1) SELECT * FROM c":               false,
		"WITH c AS (SELECT 1) INSERT INTO t SELECT * FROM c": true,
		"INSERT INTO t VALUES (1)":                           true, "UPDATE t SET a = 1": true, "DELETE FROM t": true,
		"PRAGMA journal_mode": false, "PRAGMA user_version = 3": true, "CREATE TABLE x (a)": true, "": true,
	}
	for q, want := range tests {
		if got := IsWrite(q); got != want {
			t.Errorf("IsWrite(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestConfigDefaultsValidateWarnings(t *testing.T) {
	d := Config{}.WithDefaults()
	if d.MailboxSize != 256 || d.MaxBatch != 64 || d.JournalMode != "WAL" || d.Synchronous != "NORMAL" ||
		d.CacheKiB != 256 || d.BusyTimeout != 5*time.Second || d.StmtCache != 64 || d.ReadConns != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	if got := (Config{JournalMode: "delete", Synchronous: "full"}).WithDefaults(); got.JournalMode != "DELETE" || got.Synchronous != "FULL" {
		t.Fatalf("not upper-cased: %+v", got)
	}
	tests := []struct {
		name     string
		c        Config
		valid    bool
		warnings int
	}{
		{"zero value", Config{}, true, 0},
		{"every journal mode", Config{JournalMode: "truncate"}, true, 0},
		{"unknown journal mode", Config{JournalMode: "wal2"}, false, 0},
		{"unknown synchronous", Config{Synchronous: "sometimes"}, false, 0},
		{"read pool with WAL", Config{ReadConns: 2}, true, 0},
		{"read pool without WAL", Config{ReadConns: 2, JournalMode: "DELETE"}, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, tc.valid)
			}
			if n := len(tc.c.Warnings()); n != tc.warnings {
				t.Fatalf("Warnings() = %d, want %d", n, tc.warnings)
			}
		})
	}
}

func TestWaitGivesUpWithTheContext(t *testing.T) {
	r := &Request{reply: make(chan Result, 1)}
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if res := r.Wait(c); !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("got %v", res.Err)
	}
}
