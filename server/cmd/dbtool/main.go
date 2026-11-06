// Command dbtool inspects and repairs the durable copies of databases in object storage (object-storage
// guide O3, plan OS-12). Runbook: knowledge/writeback/2026-11-05-storage-runbook.md.
//
//	dbtool [flags] inspect <key>            manifest, generations with metadata, what gc would delete
//	dbtool [flags] verify <key>             download the current generation and check its sha256
//	dbtool [flags] orphans <key>            what gc would delete for key (a dry run)
//	dbtool [flags] gc [-dry-run] (-key K | -shard SS | -all)
//	dbtool [flags] rebuild-manifest -gen G -epoch E [-object NAME] [-if-version V] <key>
//	dbtool [flags] restore [-gen G] [-object NAME] <key> <new file>
//	dbtool [flags] import <file> -tenant T -app A -schema F [-placement-dir D -vm V]
//
// restore and import (replace-platform plan phase 4) print one JSON line.
// Credentials come from the environment (DBTOOL_S3_ACCESS_KEY, DBTOOL_S3_SECRET_KEY), never from flags or
// files. Results go to stdout; a JSON log (UTC, log IDs) goes to stderr. Exit codes: 0 ok, 1 expected failure
// (message + log ID; e.g. verify found corruption), 2 usage, 70 bug, 130 interrupted.
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
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/objstore"
)

const (
	exitOK, exitError, exitUsage, exitBug, exitInterrupted = 0, 1, 2, 70, 130
	module                                                 = "dbtool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage: dbtool [flags] <command> [command flags] [key]
commands:
  inspect <key>        manifest, generations with metadata, what gc would delete
  verify <key>         download the current generation and check its sha256
  orphans <key>        what gc would delete for key (a dry run)
  gc [-dry-run] (-key K | -shard SS | -all)
  rebuild-manifest -gen G -epoch E [-object NAME] [-if-version V] <key>
  restore [-gen G] [-object NAME] <key> <new file>
                       write the current generation (or generation G, e.g. a backup's) to a new local file
  import <file> -tenant T -app A -schema F [-placement-dir D -vm V]
                       make an existing SQLite file (e.g. Deno-written) the first generation of tenant:app
environment: DBTOOL_S3_ACCESS_KEY, DBTOOL_S3_SECRET_KEY (required)
flags:`

// realMain runs one command and returns the exit code. Every error is logged once, here, with a log ID.
func realMain(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("dbtool", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "localhost:8333", "S3 endpoint host:port")
	bucket := fs.String("bucket", "coldload", "bucket")
	root := fs.String("root", "", "prefix before dbs/ (tests)")
	keep := fs.Int("keep", durable.DefaultGCPolicy.Keep, "gc: generations kept below the current one (K)")
	grace := fs.Duration("grace", durable.DefaultGCPolicy.Grace, "gc: grace window (2 × the agents' write-back timeout)")
	timeout := fs.Duration("timeout", 10*time.Minute, "deadline for the whole command")
	fs.Usage = func() { fmt.Fprintln(stderr, usage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return exitUsage
	}
	run, ok := parseCommand(fs.Arg(0), fs.Args()[1:], stderr)
	if !ok {
		fs.Usage()
		return exitUsage
	}

	log := logx.New(stderr, slog.LevelInfo)
	defer func() {
		if p := recover(); p != nil {
			code = bug(log, stderr, fmt.Errorf("panic: %v", p), string(debug.Stack()))
		}
	}()
	access, secret := getenv("DBTOOL_S3_ACCESS_KEY"), getenv("DBTOOL_S3_SECRET_KEY")
	if access == "" || secret == "" {
		fmt.Fprintln(stderr, "dbtool: set DBTOOL_S3_ACCESS_KEY and DBTOOL_S3_SECRET_KEY")
		return exitUsage
	}
	st, err := objstore.New(objstore.Config{Endpoint: *endpoint, AccessKey: access, SecretKey: secret, Bucket: *bucket, Logger: log})
	if err != nil {
		return report(log, stderr, errs.Wrap(module, err, "cannot connect to object storage"))
	}
	tmp, err := os.MkdirTemp("", "dbtool-")
	if err != nil {
		return report(log, stderr, errs.Wrap(module, err, "cannot create a scratch directory"))
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	k := *keep
	if k == 0 {
		k = -1 // GCPolicy reads 0 as "the default"; the flag's 0 means K = 0
	}
	t := &tool{s: objstore.NewDurable(st, *root), out: stdout, now: time.Now, tmp: tmp,
		gc: durable.GCPolicy{Keep: k, Grace: *grace}.Normalize()}
	if err := run(ctx, t); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			fmt.Fprintln(stderr, "dbtool: interrupted")
			return exitInterrupted
		}
		return report(log, stderr, err)
	}
	return exitOK
}

// parseCommand parses a command and its own flags; false means bad usage (already explained on stderr).
func parseCommand(name string, args []string, stderr io.Writer) (func(context.Context, *tool) error, bool) {
	fs := flag.NewFlagSet("dbtool "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyArg := func() (string, bool) {
		if fs.NArg() != 1 {
			fmt.Fprintf(stderr, "dbtool %s: want exactly one key\n", name)
			return "", false
		}
		if err := durable.ValidateKey(fs.Arg(0)); err != nil {
			fmt.Fprintf(stderr, "dbtool %s: %v\n", name, err)
			return "", false
		}
		return fs.Arg(0), true
	}
	switch name {
	case "inspect", "verify", "orphans":
		if fs.Parse(args) != nil {
			return nil, false
		}
		key, ok := keyArg()
		if !ok {
			return nil, false
		}
		cmd := map[string]func(*tool, context.Context, string) error{
			"inspect": (*tool).inspect,
			"verify":  (*tool).verify,
			"orphans": func(t *tool, ctx context.Context, key string) error { return t.sweep(ctx, one(key), true) },
		}[name]
		return func(ctx context.Context, t *tool) error { return cmd(t, ctx, key) }, true

	case "gc":
		dry := fs.Bool("dry-run", false, "only report what would be deleted")
		key := fs.String("key", "", "sweep one key")
		shard := fs.String("shard", "", "sweep one shard (two hex digits)")
		all := fs.Bool("all", false, "sweep every shard")
		if fs.Parse(args) != nil || fs.NArg() != 0 {
			return nil, false
		}
		var shards []string
		switch {
		case *key != "" && *shard == "" && !*all:
			if err := durable.ValidateKey(*key); err != nil {
				fmt.Fprintf(stderr, "dbtool gc: %v\n", err)
				return nil, false
			}
			return func(ctx context.Context, t *tool) error { return t.sweep(ctx, one(*key), *dry) }, true
		case *shard != "" && *key == "" && !*all:
			if !durable.ValidShard(*shard) {
				fmt.Fprintf(stderr, "dbtool gc: shard %q is not two lower-case hex digits\n", *shard)
				return nil, false
			}
			shards = []string{*shard}
		case *all && *key == "" && *shard == "":
			shards = durable.Shards()
		default:
			fmt.Fprintln(stderr, "dbtool gc: give exactly one of -key, -shard, -all")
			return nil, false
		}
		return func(ctx context.Context, t *tool) error {
			return t.sweep(ctx, func(fn func(string) error) error {
				for _, sh := range shards {
					if err := t.s.ListKeys(ctx, sh, fn); err != nil {
						return err
					}
				}
				return nil
			}, *dry)
		}, true

	case "rebuild-manifest":
		var a rebuildArgs
		fs.Uint64Var(&a.gen, "gen", 0, "generation to publish (required, >= 1)")
		fs.Uint64Var(&a.epoch, "epoch", 0, "the owner's epoch, from the placement row (required)")
		fs.StringVar(&a.object, "object", "", "which object of the generation, when two epochs uploaded it")
		fs.StringVar(&a.ifVersion, "if-version", "", "the manifest version inspect showed (required when one exists)")
		if fs.Parse(args) != nil {
			return nil, false
		}
		key, ok := keyArg()
		if !ok {
			return nil, false
		}
		if a.gen == 0 || a.epoch == 0 {
			fmt.Fprintln(stderr, "dbtool rebuild-manifest: -gen and -epoch are required")
			return nil, false
		}
		a.key = key
		return func(ctx context.Context, t *tool) error { return t.rebuildManifest(ctx, a) }, true

	case "restore":
		var a restoreArgs
		fs.Uint64Var(&a.gen, "gen", 0, "the generation to restore (a backup's answer); default the current one")
		fs.StringVar(&a.object, "object", "", "which object of the generation, when two epochs uploaded it")
		if fs.Parse(args) != nil {
			return nil, false
		}
		if fs.NArg() != 2 {
			fmt.Fprintln(stderr, "dbtool restore: want a key and a new file path")
			return nil, false
		}
		if err := durable.ValidateKey(fs.Arg(0)); err != nil {
			fmt.Fprintf(stderr, "dbtool restore: %v\n", err)
			return nil, false
		}
		a.key, a.target = fs.Arg(0), fs.Arg(1)
		return func(ctx context.Context, t *tool) error { return t.restore(ctx, a) }, true

	case "import":
		var a importArgs
		fs.StringVar(&a.tenant, "tenant", "", "the tenant (required)")
		fs.StringVar(&a.app, "app", "", "the app (required); the key is tenant:app")
		fs.StringVar(&a.schema, "schema", "", "the app's schema file, in fingerprint form (required)")
		fs.StringVar(&a.placementDir, "placement-dir", "", "the single-node placement authority (agentd's placement_dir)")
		fs.StringVar(&a.vm, "vm", "", "the owner the placement row names (agentd's vm); required with -placement-dir")
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") { // the file may come first: import <file> -tenant ...
			a.file, args = args[0], args[1:]
		}
		if fs.Parse(args) != nil {
			return nil, false
		}
		if a.file == "" && fs.NArg() == 1 {
			a.file = fs.Arg(0)
		} else if fs.NArg() != 0 || a.file == "" {
			fmt.Fprintln(stderr, "dbtool import: want exactly one file")
			return nil, false
		}
		if problem := a.check(); problem != "" {
			fmt.Fprintln(stderr, "dbtool import: "+problem)
			return nil, false
		}
		return func(ctx context.Context, t *tool) error { return t.importFile(ctx, a) }, true
	}
	fmt.Fprintf(stderr, "dbtool: unknown command %q\n", name)
	return nil, false
}

// one iterates over a single key.
func one(key string) func(func(string) error) error {
	return func(fn func(string) error) error { return fn(key) }
}

// report logs an expected failure once with a log ID and tells the operator; a malformed error is a bug.
func report(log *slog.Logger, stderr io.Writer, err error) int {
	if e, ok := errs.Outermost(err); !ok || e.Module == "" {
		return bug(log, stderr, err, errs.Stack(err))
	}
	id := logx.NewID()
	log.Warn("DBTOOL.FAILED", "log_id", id, "error", err.Error(), "chain", errs.Chain(err))
	fmt.Fprintf(stderr, "error: %v [log ID %s]\n", err, id)
	return exitError
}

func bug(log *slog.Logger, stderr io.Writer, err error, stack string) int {
	id := logx.NewID()
	log.Error("DBTOOL.BUG", "log_id", id, "error", err.Error(), "chain", errs.Chain(err), "stack", stack)
	fmt.Fprintf(stderr, "error: something unexpected went wrong. This is a bug; please report log ID %s\n", id)
	return exitBug
}
