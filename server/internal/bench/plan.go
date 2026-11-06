// Package bench runs a load configuration against a directory of database actors and streams what happened to
// CSV files.
//
// plan.go holds the pure decisions (how a result is classified, what a record says, where files go, what the
// plan prints); run.go does the work (goroutines, timers, files).
package bench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/load"
	"github.com/nickstrad/mansio/server/internal/outcome"
	"github.com/nickstrad/mansio/server/internal/schema"
	"github.com/nickstrad/mansio/server/internal/stats"
	"github.com/nickstrad/mansio/server/internal/workload"
)

// Classify maps an actor result to its outcome. A statement timeout wins over the error it caused.
func Classify(res actor.Result) outcome.Code {
	switch {
	case res.Err == nil:
		return outcome.OK
	case res.TimedOut:
		return outcome.StmtTimeout
	case errors.Is(res.Err, actor.ErrMailboxFull):
		return outcome.Shed
	case errors.Is(res.Err, actor.ErrExpired):
		return outcome.Expired
	case errors.Is(res.Err, actor.ErrAborted):
		return outcome.Aborted
	case errors.Is(res.Err, actor.ErrClosed):
		return outcome.Closed
	case errors.Is(res.Err, context.DeadlineExceeded):
		return outcome.ClientTimeout
	}
	return outcome.Error
}

// job is one attempt of one request.
type job struct {
	phase     int16
	pick      load.Pick
	attempt   uint8
	first     time.Time     // when the first attempt was scheduled
	sched     time.Time     // when this attempt was scheduled
	prevDelay time.Duration // backoff before this attempt
}

func newJob(phase int, pk load.Pick, now time.Time) job {
	return job{phase: int16(phase), pick: pk, attempt: 1, first: now, sched: now}
}

// retryOf is the next attempt of j after delay.
func retryOf(j job, delay time.Duration) job {
	j.attempt++
	j.prevDelay = delay
	return j
}

// base is the part of a record that does not depend on the reply.
func (j job) base(start time.Time) stats.Record {
	return stats.Record{
		Phase: j.phase, Client: uint8(j.pick.Client), Attempt: j.attempt, DB: int32(j.pick.DB),
		Class: uint8(workload.Index(j.pick.Class)), Write: j.pick.Write, ExpectSlow: j.pick.ExpectSlow,
		SchedNs: j.sched.Sub(start).Nanoseconds(),
	}
}

// answered is the record of an attempt sent at send and answered at done. Final is decided by the caller.
func answered(j job, res actor.Result, send, done, start time.Time) stats.Record {
	r := j.base(start)
	r.Outcome = Classify(res)
	r.Reopened = res.Reopened
	r.Batch = int16(min(res.Batch, math.MaxInt16))
	r.Rows = int32(min(res.Rows, math.MaxInt32))
	r.SendNs = send.Sub(start).Nanoseconds()
	r.LatencyNs = done.Sub(send).Nanoseconds()
	r.E2ENs = done.Sub(j.first).Nanoseconds()
	r.QueueNs, r.ExecNs, r.CommitNs = res.QueueWait.Nanoseconds(), res.Exec.Nanoseconds(), res.Commit.Nanoseconds()
	return r
}

// notSent is the record of an attempt that found every worker busy and the queue full. It is final: the
// client never got an answer to retry on.
func notSent(j job, start time.Time) stats.Record {
	r := j.base(start)
	r.Outcome, r.Final, r.SendNs = outcome.GenFull, true, r.SchedNs
	return r
}

// replyWait is how long a worker waits for a reply: the actor drops a request still queued at its deadline,
// but one that started just before may run for up to the statement timeout.
func replyWait(cfg *load.Config) time.Duration {
	return cfg.RequestTimeout.Duration + cfg.StatementTimeout.Duration + 100*time.Millisecond
}

// queueSize is the job queue's capacity: two jobs per worker, and at least four ticks' worth of the fastest
// phase, so a burst that free workers would absorb within microseconds is not recorded as gen_full.
func queueSize(cfg *load.Config) int {
	maxQPS := 0.0
	for _, p := range cfg.Phases {
		maxQPS = max(maxQPS, p.StartQPS, p.EndQPS)
	}
	return max(cfg.Workers*2, int(math.Ceil(maxQPS/1000))*4)
}

// Resolve fills the output and database directories if the config left them empty.
func Resolve(cfg *load.Config, now time.Time, tmp string) {
	if cfg.OutDir == "" {
		cfg.OutDir = filepath.Join("results", fmt.Sprintf("%s-%s", cfg.Name, now.Format("20060102-150405")))
	}
	if cfg.DBDir == "" {
		cfg.DBDir = filepath.Join(tmp, "mansio-server", schema.NewCatalog(cfg.Seed, cfg.Schema).Fingerprint())
	}
}

// DBPath is database i's file.
func DBPath(dir string, i int) string { return filepath.Join(dir, fmt.Sprintf("db-%06d.sqlite", i)) }

// Plan describes the run in human terms, warnings first. nofile is the open-file soft limit.
func Plan(cfg *load.Config, nofile uint64) []string {
	var lines []string
	for _, w := range cfg.Warnings() {
		lines = append(lines, "warning: "+w)
	}
	a := cfg.Actor.Actor()
	if need := uint64(cfg.MaxDBs()) * 3 * uint64(1+a.ReadConns); need > nofile {
		lines = append(lines, fmt.Sprintf("warning: up to %d databases may need ~%d descriptors (db + wal + shm per "+
			"connection), above the open-file limit %d; set actor.idle_close to passivate idle databases", cfg.MaxDBs(), need, nofile))
	}
	lines = append(lines,
		fmt.Sprintf("run %q: journal_mode %s, synchronous %s, read conns %d; out %s; dbs in %s",
			cfg.Name, a.JournalMode, a.Synchronous, a.ReadConns, cfg.OutDir, cfg.DBDir),
		fmt.Sprintf("workers %d, statement timeout %s, request timeout %s, open-file limit %d",
			cfg.Workers, cfg.StatementTimeout.Duration, cfg.RequestTimeout.Duration, nofile))
	var total float64
	for i, p := range cfg.Phases {
		e := p.ExpectedRequests()
		total += e
		lines = append(lines, fmt.Sprintf("  phase %d %-12s dbs=%-6d %8.1f→%-8.1f qps %-6s %-8s ~%.0f requests, %s",
			i, p.Name, p.DBs, p.StartQPS, p.EndQPS, p.Shape, p.Duration.Duration, e, p.Distribution.Kind))
	}
	return append(lines, fmt.Sprintf("  total ~%.0f requests over %s (plus setup)", total, cfg.TotalDuration()))
}

// schemaColumns and schemaRows describe each table of a database (schemas.csv).
var schemaColumns = []string{"db", "table_name", "columns", "indexed_columns", "has_parent", "seed_rows", "types"}

func schemaRows(s *schema.Schema) [][]any {
	rows := make([][]any, 0, len(s.Tables))
	for _, t := range s.Tables {
		types := make([]string, len(t.Columns))
		indexed := 0
		for i, c := range t.Columns {
			types[i] = string(c.Type)
			if c.Indexed {
				indexed++
			}
		}
		rows = append(rows, []any{s.DB, t.Name, len(t.Columns), indexed, t.HasParent(), t.SeedRows, strings.Join(types, " ")})
	}
	return rows
}

// phaseColumns is phases.csv's header; phaseStats is one row.
var phaseColumns = []string{
	"phase", "name", "dbs", "added_dbs", "setup_start_ms", "seed_ms", "open_ms", "start_ms", "end_ms", "shape",
	"start_qps", "end_qps", "expected_requests", "sent", "gen_full", "distribution", "mix",
}

type phaseStats struct {
	index                  int
	grow                   actor.GrowStats
	setupStart, start, end time.Duration // since the run started
	sent, notSent          int64
}

func (s phaseStats) row(p *load.Phase) []any {
	return []any{
		s.index, p.Name, p.DBs, s.grow.Added, s.setupStart, s.grow.Seed, s.grow.Open, s.start, s.end, string(p.Shape),
		p.StartQPS, p.EndQPS, math.Round(p.ExpectedRequests()), s.sent, s.notSent, string(p.Distribution.Kind),
		load.FormatWeights(p.Mix),
	}
}
