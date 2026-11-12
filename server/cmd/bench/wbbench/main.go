// Command wbbench measures write-back: how long a warm write-back (snapshot, hash, upload, publish) and a
// write-back on passivation take by database size, how long writes stall meanwhile, how many store requests a
// write-back sends and what they cost at S3 list prices, and what the dirty check and a maintenance tick cost
// per database.
//
// The store is the in-memory fake (durabletest.Store), so "upload" is a memory copy: it shows the host's
// own cost, not S3's. For S3 transfer rates see docs/notes/coldload (~250 MB/s against local SeaweedFS).
//
// Exit codes: 0 done, 1 failed, 2 bad usage, 70 bug, 130 interrupted.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/panicx"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

const (
	exitOK, exitError, exitUsage, exitBug, exitInterrupted = 0, 1, 2, 70, 130
	module                                                 = "wbbench"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

type config struct {
	sizes    []int64
	reps     int
	tickDBs  int
	dir, out string
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("size %q: want a positive integer with optional K, M or G", s)
	}
	return n * mult, nil
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("wbbench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sizes := fs.String("sizes", "64K,1M,16M,128M", "database sizes, comma-separated (K, M, G suffixes)")
	reps := fs.Int("reps", 3, "repetitions per size")
	tick := fs.Int("tick-dbs", 1000, "warm clean databases for the Tick / dirty-check measurement")
	dir := fs.String("dir", "", "scratch directory (default: a new temp dir, removed at the end)")
	out := fs.String("out", "", "CSV file for the per-run rows (optional)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	c := config{reps: *reps, tickDBs: *tick, dir: *dir, out: *out}
	for _, s := range strings.Split(*sizes, ",") {
		n, err := parseSize(strings.TrimSpace(s))
		if err != nil {
			return config{}, err
		}
		c.sizes = append(c.sizes, n)
	}
	if c.reps < 1 || c.tickDBs < 1 {
		return config{}, fmt.Errorf("-reps and -tick-dbs must be >= 1")
	}
	return c, nil
}

// realMain runs the benchmark and returns the exit code. Errors are classified and logged once, here.
func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	log := logx.New(stderr, slog.LevelInfo)
	defer func() {
		if p := panicx.From(recover()); p != nil {
			id := logx.NewID()
			log.Error("bug", "log_id", id, "panic", fmt.Sprint(p.Value), "stack", p.Stack)
			fmt.Fprintf(stderr, "wbbench: internal error (a bug); log ID %s\n", id)
			code = exitBug
		}
	}()
	cfg, err := parseArgs(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "wbbench: %v\n", err)
		return exitUsage
	}
	if err := run(ctx, cfg, stdout, log); err != nil {
		id := logx.NewID()
		log.Error("failed", "log_id", id, "chain", errs.Chain(err), "stack", errs.Stack(err))
		if ctx.Err() != nil {
			fmt.Fprintf(stderr, "wbbench: interrupted; log ID %s\n", id)
			return exitInterrupted
		}
		if _, ok := errs.Outermost(err); !ok {
			fmt.Fprintf(stderr, "wbbench: unexpected error (a bug): %v; log ID %s\n", err, id)
			return exitBug
		}
		fmt.Fprintf(stderr, "wbbench: %v; log ID %s\n", err, id)
		return exitError
	}
	return exitOK
}

type row struct {
	size, bytes                            int64
	rep                                    int
	snapshot, hash, upload, publish, total time.Duration
	maxWriteStall                          time.Duration
	passivate                              time.Duration
	requests                               int64   // store requests of the warm write-back (OS-08)
	usdPerMillion                          float64 // their cost at S3 list prices, per million write-backs
}

func ms(d time.Duration) string { return strconv.FormatFloat(float64(d)/1e6, 'f', 3, 64) }

func run(ctx context.Context, cfg config, stdout io.Writer, log *slog.Logger) error {
	dir := cfg.dir
	if dir == "" {
		d, err := os.MkdirTemp("", "wbbench-")
		if err != nil {
			return errs.Wrap(module, err, "cannot create a scratch directory")
		}
		dir = d
		defer os.RemoveAll(d)
	}
	var w *csv.Writer
	if cfg.out != "" {
		f, err := os.Create(cfg.out)
		if err != nil {
			return errs.Wrap(module, err, "cannot create %s", cfg.out)
		}
		defer f.Close()
		w = csv.NewWriter(f)
		defer w.Flush()
		w.Write([]string{"size", "bytes", "rep", "snapshot_ms", "hash_ms", "upload_ms", "publish_ms", "writeback_ms",
			"max_write_stall_ms", "passivate_ms", "requests", "usd_per_million"})
	}
	fmt.Fprintf(stdout, "# wbbench %s, %s/%s, GOMAXPROCS=%d, fake in-memory store\n", time.Now().UTC().Format(time.RFC3339),
		runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0))
	fmt.Fprintf(stdout, "%10s %10s %4s %10s %8s %8s %8s %10s %10s %10s %4s %8s\n", "size", "bytes", "rep", "snapshot", "hash", "upload",
		"publish", "writeback", "maxstall", "passivate", "reqs", "$/M")
	for _, size := range cfg.sizes {
		for rep := range cfg.reps {
			if ctx.Err() != nil {
				return errs.Wrap(module, ctx.Err(), "interrupted")
			}
			r, err := measure(ctx, dir, size, rep)
			if err != nil {
				return err
			}
			usd := strconv.FormatFloat(r.usdPerMillion, 'f', 2, 64)
			fmt.Fprintf(stdout, "%10d %10d %4d %10s %8s %8s %8s %10s %10s %10s %4d %8s\n", r.size, r.bytes, r.rep, ms(r.snapshot), ms(r.hash),
				ms(r.upload), ms(r.publish), ms(r.total), ms(r.maxWriteStall), ms(r.passivate), r.requests, usd)
			if w != nil {
				w.Write([]string{strconv.FormatInt(r.size, 10), strconv.FormatInt(r.bytes, 10), strconv.Itoa(r.rep), ms(r.snapshot),
					ms(r.hash), ms(r.upload), ms(r.publish), ms(r.total), ms(r.maxWriteStall), ms(r.passivate),
					strconv.FormatInt(r.requests, 10), usd})
			}
		}
	}
	return measureTick(ctx, dir, cfg.tickDBs, stdout)
}

const ddl = `CREATE TABLE t (id INTEGER PRIMARY KEY, b BLOB NOT NULL)`

// seed builds a database of about size bytes (4000-byte random blobs).
func seed(path string, size int64) error {
	rows := max(1, size/4096)
	return actor.Seed(path, actor.Config{}, ddl, func(exec func(string, ...any) error) error {
		return exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?) INSERT INTO t (b) SELECT randomblob(4000) FROM n`, rows)
	})
}

func newHost(dir string, s *durabletest.Store) (*vactor.Host, error) {
	h, err := vactor.NewHost(nil, actordb.Opener{Cfg: actor.Config{ReadConns: 2}}, vactor.Options{CacheDir: dir, Store: s, CreateMissing: true})
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot create host")
	}
	return h, nil
}

func measure(ctx context.Context, dir string, size int64, rep int) (row, error) {
	r := row{size: size, rep: rep}
	src := filepath.Join(dir, "seed.db")
	vactor.RemoveDB(src)
	if err := seed(src, size); err != nil {
		return r, errs.Wrap(module, err, "cannot seed %d bytes", size)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return r, errs.Wrap(module, err, "cannot read seed")
	}
	s := durabletest.NewStore()
	s.Seed("bench/db", 0, data)
	data = nil
	h, err := newHost(filepath.Join(dir, "cache"), s)
	if err != nil {
		return r, err
	}
	defer h.Close()
	res, err := h.Get(ctx, "bench/db")
	if err != nil {
		return r, errs.Wrap(module, err, "cannot activate")
	}
	a := res.DB.(*actordb.DB).Actor()
	if x := a.Do(ctx, `INSERT INTO t (b) VALUES (randomblob(100))`, true); x.Err != nil {
		return r, errs.Wrap(module, x.Err, "cannot write")
	}

	// A writer runs during the warm write-back; its slowest write is the stall the snapshot causes.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var maxStall time.Duration
	var werr error
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			t := time.Now()
			if x := a.Do(ctx, `INSERT INTO t (b) VALUES (randomblob(100))`, true); x.Err != nil {
				werr = x.Err
				return
			}
			maxStall = max(maxStall, time.Since(t))
			time.Sleep(200 * time.Microsecond)
		}
	})
	time.Sleep(5 * time.Millisecond)
	before := s.RequestCounts()
	t := time.Now()
	wb, err := h.WriteBack(ctx, "bench/db")
	r.total = time.Since(t)
	sent := durable.SubCounts(s.RequestCounts(), before)
	r.requests, r.usdPerMillion = durable.Requests(sent), durable.RequestCost(sent, durable.S3Standard)*1e6
	time.Sleep(5 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err != nil {
		return r, errs.Wrap(module, err, "warm write-back failed")
	}
	if werr != nil {
		return r, errs.Wrap(module, werr, "a write during the write-back failed")
	}
	r.bytes, r.snapshot, r.hash, r.upload, r.publish, r.maxWriteStall = wb.Bytes, wb.Snapshot, wb.Hash, wb.Upload, wb.Publish, maxStall

	t = time.Now() // the writer left the db dirty: passivation closes, hashes and uploads the file
	if err := h.Passivate(ctx, "bench/db"); err != nil {
		return r, errs.Wrap(module, err, "passivation failed")
	}
	r.passivate = time.Since(t)
	return r, nil
}

// measureTick times the dirty check (Commits), a no-op WriteBack on a clean db, and a Tick over n warm,
// clean databases with a policy that plans nothing.
func measureTick(ctx context.Context, dir string, n int, stdout io.Writer) error {
	s := durabletest.NewStore()
	h, err := vactor.NewHost(nil, actordb.Opener{}, vactor.Options{CacheDir: filepath.Join(dir, "tick"), Store: s, CreateMissing: true,
		Policy: vactor.Policy{WriteBackCommits: 1 << 30, WriteBackEvery: time.Hour, IdleAfter: time.Hour, MaxActive: n + 1}})
	if err != nil {
		return errs.Wrap(module, err, "cannot create host")
	}
	defer h.Close()
	var first *actordb.DB
	for i := range n {
		res, err := h.Get(ctx, fmt.Sprintf("tick/%05d", i))
		if err != nil {
			return errs.Wrap(module, err, "cannot activate tick db %d", i)
		}
		if first == nil {
			first = res.DB.(*actordb.DB)
		}
	}
	const calls = 10_000_000
	t := time.Now()
	var sink uint64
	for range calls {
		sink += first.Commits()
	}
	commitsNs := float64(time.Since(t).Nanoseconds()) / calls
	const wbCalls = 1_000_000
	t = time.Now()
	for range wbCalls {
		if _, err := h.WriteBack(ctx, "tick/00000"); err != nil {
			return errs.Wrap(module, err, "clean write-back failed")
		}
	}
	wbNs := float64(time.Since(t).Nanoseconds()) / wbCalls
	const ticks = 20
	t = time.Now()
	for range ticks {
		rep, err := h.Tick(ctx)
		if err != nil {
			return errs.Wrap(module, err, "tick failed")
		}
		if len(rep.Plan.Passivate)+len(rep.Plan.WriteBack) != 0 {
			return errs.Wrap(module, nil, "tick planned work on clean dbs: %+v", rep.Plan)
		}
	}
	tick := time.Since(t) / ticks
	fmt.Fprintf(stdout, "# dirty check (actor.Commits): %.2f ns/call (sink %d)\n", commitsNs, sink%2)
	fmt.Fprintf(stdout, "# WriteBack of a clean warm db (lookup + lock + dirty check): %.1f ns/call\n", wbNs)
	fmt.Fprintf(stdout, "# Tick over %d warm clean dbs (candidates + 2 stats each + plan): %s ms (%.2f µs/db)\n", n, ms(tick),
		float64(tick.Microseconds())/float64(n))
	return nil
}
