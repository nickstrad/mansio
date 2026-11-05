// Command coldload measures how fast one Go process can turn SQLite files stored in object storage
// (SeaweedFS, S3 API) into ready virtual actors, by file size and by how many activations run at once.
//
//	coldload -config configs/coldload/sizes.json plan|seed|run|all|clean
//
// Human progress goes to stderr; a JSON log (UTC, log IDs) goes to <out_dir>/coldload.log. Exit codes:
// 0 ok, 1 expected failure (message + log ID), 2 usage, 70 bug (please report the log ID), 130 interrupted.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/nickstrad/ts-db-go/internal/coldload"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/objstore"
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

// realMain runs one command and returns the exit code. Every error is logged once, here, with a log ID.
func realMain(ctx context.Context, args []string, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("coldload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "configs/coldload/smoke.json", "experiment config")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: coldload [-config file] plan|seed|run|all|clean")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cmd := fs.Arg(0)
	if cmd == "" {
		cmd = "all"
	}
	if fs.NArg() > 1 || !map[string]bool{"plan": true, "seed": true, "run": true, "all": true, "clean": true}[cmd] {
		fs.Usage()
		return exitUsage
	}

	rep := reporter{logger: logx.New(io.Discard, slog.LevelInfo), stderr: stderr}
	defer func() {
		if p := recover(); p != nil {
			code = rep.bug(fmt.Errorf("panic: %v", p), string(debug.Stack()))
		}
	}()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return rep.report(err)
	}
	if cmd != "plan" {
		logFile, err := openLog(cfg.OutDir)
		if err != nil {
			return rep.report(err)
		}
		defer logFile.Close()
		rep.logger, rep.logs = logx.New(logFile, slog.LevelInfo), logFile.Name()
		rep.logger.Info("start", "command", cmd, "config", *cfgPath)
	}
	app := &app{ctx: ctx, cfg: cfg, log: rep.logger, out: stderr}

	switch cmd {
	case "plan":
		app.plan()
		return exitOK
	case "clean":
		err = app.withStore(app.clean)
	case "seed":
		err = app.withStore(app.seed)
	case "run":
		app.plan()
		err = app.withStore(app.run)
	case "all":
		app.plan()
		err = app.withStore(func(ctx context.Context, st *objstore.Store) error {
			if err := app.seed(ctx, st); err != nil {
				return err
			}
			return app.run(ctx, st)
		})
	}
	if err == nil {
		rep.logger.Info("done", "command", cmd)
		return exitOK
	}
	if ctx.Err() != nil {
		rep.logger.Warn("interrupted", "command", cmd, "error", err.Error())
		fmt.Fprintln(stderr, "interrupted; local copies removed")
		return exitInterrupted
	}
	return rep.report(err)
}

// app holds what every command needs.
type app struct {
	ctx context.Context
	cfg coldload.Config
	log *slog.Logger
	out io.Writer // human progress
}

func (a *app) withStore(fn func(context.Context, *objstore.Store) error) error {
	st, err := objstore.New(objstore.Config{
		Endpoint: a.cfg.S3.Endpoint, AccessKey: a.cfg.S3.AccessKey, SecretKey: a.cfg.S3.SecretKey,
		Bucket: a.cfg.S3.Bucket, MaxConns: max(1024, 2*coldload.MaxConcurrency(a.cfg.Levels())),
	})
	if err != nil {
		return errs.Wrap(module, err, "cannot connect to object storage")
	}
	return fn(a.ctx, st)
}

func (a *app) progress(format string, args ...any) { fmt.Fprintf(a.out, format+"\n", args...) }

// loadConfig reads and validates a config file (the only place input is validated).
func loadConfig(path string) (coldload.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return coldload.Config{}, errs.Wrap(module, err, "cannot read config %s", path)
	}
	var c coldload.Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return coldload.Config{}, errs.Wrap(module, err, "cannot parse config %s", path)
	}
	if err := c.Normalize(); err != nil {
		return coldload.Config{}, err
	}
	return c, nil
}

func openLog(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.Wrap(module, err, "cannot create output directory %s", dir)
	}
	f, err := os.OpenFile(filepath.Join(dir, "coldload.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot open log in %s", dir)
	}
	return f, nil
}

func (a *app) plan() {
	c := a.cfg
	var stored int64
	for _, s := range c.Sizes {
		stored += s.Bytes * int64(s.Count)
	}
	a.progress("coldload %q: bucket %s, prefix %s, ~%s stored", c.Name, c.S3.Bucket, c.Prefix, coldload.Human(stored))
	a.progress("  first query: %s\n  warm: %v\n  keep_local=%v read_conns=%d dup_callers=%d",
		c.FirstQuery, c.WarmQueries, c.KeepLocal, c.ReadConns, c.DupCallers)
	for _, s := range c.Sizes {
		n := min(c.PerLevel, s.Count)
		a.progress("  %-8s %10s × %-5d  levels c=%v, %d activations each (%s fetched per level)",
			s.Name, coldload.Human(s.Bytes), s.Count, s.Concurrency, n, coldload.Human(s.Bytes*int64(n)))
	}
}

func (a *app) clean(ctx context.Context, st *objstore.Store) error {
	n, err := st.DeletePrefix(ctx, a.cfg.Prefix+"/")
	if err != nil {
		return errs.Wrap(module, err, "cannot clean bucket %s", a.cfg.S3.Bucket)
	}
	if err := os.RemoveAll(a.cfg.CacheDir); err != nil {
		return errs.Wrap(module, err, "cannot remove %s", a.cfg.CacheDir)
	}
	a.log.Info("clean", "deleted_objects", n, "bucket", a.cfg.S3.Bucket, "prefix", a.cfg.Prefix)
	a.progress("clean: deleted %d objects under %s/%s/, removed %s", n, a.cfg.S3.Bucket, a.cfg.Prefix, a.cfg.CacheDir)
	return nil
}

// reporter classifies an error at the top level and logs it once (Cox-Buday ch. 5): an outermost error from
// a module this command calls is an expected failure with a message for the user; anything else is a bug.
type reporter struct {
	logger *slog.Logger
	stderr io.Writer
	logs   string // log file, shown for bugs
}

var wellFormed = map[string]bool{module: true, "coldload": true}

func (r reporter) report(err error) int {
	if e, ok := errs.Outermost(err); ok && wellFormed[e.Module] {
		id := r.log(slog.LevelWarn, "command failed", err, errs.Stack(err))
		fmt.Fprintf(r.stderr, "error: %s [log ID %s]\n", e.Error(), id)
		return exitError
	}
	return r.bug(err, errs.Stack(err))
}

func (r reporter) bug(err error, stack string) int {
	id := r.log(slog.LevelError, "unexpected error (bug)", err, stack)
	where := r.logs
	if where == "" {
		where = "stderr above"
		fmt.Fprintf(r.stderr, "%v\n%s\n", err, stack)
	}
	fmt.Fprintf(r.stderr, "error: something unexpected went wrong. This is a bug; please report log ID %s (details in %s)\n", id, where)
	return exitBug
}

func (r reporter) log(level slog.Level, msg string, err error, stack string) string {
	id := logx.NewID()
	attrs := []slog.Attr{slog.String("log_id", id), slog.String("error", err.Error()), slog.Any("chain", errs.Chain(err))}
	if stack != "" {
		attrs = append(attrs, slog.String("stack", stack))
	}
	r.logger.LogAttrs(context.Background(), level, msg, attrs...)
	return id
}
