// Command sqlbench drives many SQLite databases, one goroutine (actor) per database, from a JSON config.
//
// It grows the number of databases phase by phase, sends generated SQL at a scheduled rate (open loop: the
// schedule does not wait for slow replies), and streams per-request records and resource samples to CSV files
// in the output directory. Analyze them afterwards with DuckDB: see queries/report.sql and scripts/report.sh.
//
// Exit codes: 0 done, 1 failed (message and log ID on stderr), 2 bad usage, 70 bug (report the log ID),
// 130 interrupted (files written so far are complete and readable).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickstrad/ts-db-go/internal/bench"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/load"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/panicx"
)

const (
	exitOK, exitError, exitUsage, exitBug, exitInterrupted = 0, 1, 2, 70, 130
	module                                                 = "cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// realMain runs sqlbench and returns the exit code. Every error is classified and logged once, here.
func realMain(ctx context.Context, args []string, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("sqlbench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "configs/ramp.json", "benchmark config (JSON)")
	outDir := fs.String("out", "", "output directory (overrides the config)")
	dbDir := fs.String("db-dir", "", "database directory (overrides the config)")
	dryRun := fs.Bool("dry-run", false, "print the plan and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "sqlbench: unexpected arguments %q\n", fs.Args())
		fs.Usage()
		return exitUsage
	}

	// Progress goes to stderr (and, once it exists, the log file); failures are reported by rep.
	human := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(human)
	rep := reporter{stderr: stderr}
	defer func() {
		if p := panicx.From(recover()); p != nil {
			code = rep.bug(p, p.Stack)
		}
	}()

	cfg, err := load.LoadFile(*cfgPath)
	if err != nil {
		return rep.report(err)
	}
	if *outDir != "" {
		cfg.OutDir = *outDir
	}
	if *dbDir != "" {
		cfg.DBDir = *dbDir
	}
	bench.Resolve(cfg, time.Now(), os.TempDir())
	if *dryRun {
		for _, line := range bench.Plan(cfg, bench.NoFileLimit()) {
			fmt.Fprintln(stderr, line)
		}
		return exitOK
	}

	logFile, err := openLog(cfg.OutDir)
	if err != nil {
		return rep.report(err)
	}
	defer logFile.Close()
	file := logx.New(logFile, slog.LevelInfo)
	log = slog.New(slog.NewMultiHandler(human, file.Handler()))
	rep.detail, rep.logs = file, logFile.Name()
	log.Info("start", "config", *cfgPath, "out", cfg.OutDir)

	sum, err := bench.Run(ctx, cfg, log)
	for _, c := range sum.Clients {
		log.Info("client", "name", c.Name, "first_attempts", c.Firsts, "retries", c.Retries)
	}
	if sum.OutDir != "" {
		log.Info("done", "attempts", sum.Attempts, "records_written", sum.Written, "records_dropped", sum.Dropped,
			"retries_scheduled", sum.RetriesScheduled, "retries_lost", sum.RetriesLost, "out", sum.OutDir,
			"analyze", "scripts/report.sh "+sum.OutDir)
	}
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		log.Warn("interrupted", "error", err.Error())
		fmt.Fprintf(stderr, "interrupted; files in %s are complete up to the stop\n", cfg.OutDir)
		return exitInterrupted
	}
	return rep.report(err)
}

func openLog(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.Wrap(module, err, "cannot create output directory").With("dir", dir)
	}
	f, err := os.OpenFile(filepath.Join(dir, "sqlbench.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open log file").With("dir", dir)
	}
	return f, nil
}

// reporter classifies an error at the top level and reports it once: an outermost error from a module this
// command calls is an expected failure with a message for the user; anything else is a bug. The user gets one
// line; the details (chain, stack) go to the log file under a log ID. Before the log file exists there is
// nowhere to look an ID up, so the line has no ID and a bug's details go to stderr.
type reporter struct {
	detail *slog.Logger // the log file; nil until it exists
	stderr io.Writer
	logs   string // log file path
}

var wellFormed = map[string]bool{module: true, "bench": true, "load": true, "actor": true, "stats": true}

func (r reporter) report(err error) int {
	if e, ok := errs.Outermost(err); ok && wellFormed[e.Module] {
		fmt.Fprintf(r.stderr, "error: %s%s\n", e.Error(), r.record(slog.LevelWarn, "run failed", err, errs.Stack(err)))
		return exitError
	}
	return r.bug(err, errs.Stack(err))
}

func (r reporter) bug(err error, stack string) int {
	if r.detail == nil {
		fmt.Fprintf(r.stderr, "%v\n%s\nerror: something unexpected went wrong. This is a bug; please report the details above\n", err, stack)
		return exitBug
	}
	id := r.record(slog.LevelError, "unexpected error (bug)", err, stack)
	fmt.Fprintf(r.stderr, "error: something unexpected went wrong. This is a bug; please report%s (details in %s)\n", id, r.logs)
	return exitBug
}

// record logs err with its chain and stack under a new log ID and returns " [log ID x]", or "" without a log.
func (r reporter) record(level slog.Level, msg string, err error, stack string) string {
	if r.detail == nil {
		return ""
	}
	id := logx.NewID()
	attrs := []slog.Attr{slog.String("log_id", id), slog.String("error", err.Error()), slog.Any("chain", errs.Chain(err))}
	if stack != "" {
		attrs = append(attrs, slog.String("stack", stack))
	}
	r.detail.LogAttrs(context.Background(), level, msg, attrs...)
	return " [log ID " + id + "]"
}
