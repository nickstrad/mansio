package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/client"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/retry"
	"github.com/nickstrad/ts-db-go/internal/tlsx"
)

// frontbench locator (control-plane plan CP-6 gate): closed-loop clients that know only controld. Every
// request goes through internal/client: a route from the locator (cached, refreshed ahead of the ticket TTL),
// the query at the owner, re-resolves after wrong_owner/fenced, retries with Retry-After and a budget. With
// -move-every, a mover moves a random database to another VM on that period through controld's admin API, and
// the summary reports the error rate of requests that started during a move (or in the 2 s after it).
//
// Secrets come from the environment: TSDB_API_KEY (the tenant's key) and TSDB_ADMIN_TOKEN (controld's admin
// token, needed for -setup and -move-every; with it and no TSDB_API_KEY, -setup creates a key).
//
// The workload is one table per database, fb(id integer primary key, v blob): point reads by a random id, and
// inserts of a 64-byte blob. The databases are <tenant>:fb-0 … fb-N-1; -setup creates them and the table.

type locatorFlags struct {
	locator, tenant, scheme, ca, out string
	dbs, conc                        int
	seed                             uint64
	write                            float64
	duration, warmup, timeout, move  time.Duration
	setup                            bool

	// Soak (soak.go): queries through an edge, an arrival rate, a skewed mix, and the two logs.
	edge, ackLog, events, runID string
	rate, zipf                  float64
}

// moveWindow is how long after a move returns its requests still count as "during the move": routes cached
// before the move are found stale only when used.
const moveWindow = 2 * time.Second

// LocatorSummary is one locator run (also written as JSON with -out): run's fields (Codes are client.CodeOf
// per call), plus the locator's.
type LocatorSummary struct {
	Summary
	ErrorRate  float64      `json:"error_rate"` // non-ok / requests
	Resolve    ResolveStats `json:"resolve"`
	Reroutes   int          `json:"reroutes"`    // re-resolves inside calls
	Retries    int64        `json:"retries"`     // same-route retries (budget spent)
	Moves      int          `json:"moves"`       // moves that succeeded during the measured part
	MoveFailed int          `json:"move_failed"` // moves controld refused or failed
	MoveP50ms  float64      `json:"move_p50_ms"` // move call duration
	DuringMove MoveWindow   `json:"during_move"` // requests started during a move or moveWindow after it
}

// ResolveStats are the route lookups of the measured part: hits come from the client's cache, misses wait for
// a resolve call (one per database at a time).
type ResolveStats struct {
	Hits      int     `json:"hits"`
	Misses    int     `json:"misses"`
	HitP50us  float64 `json:"hit_p50_us"`
	HitP99us  float64 `json:"hit_p99_us"`
	MissP50us float64 `json:"miss_p50_us"`
	MissP99us float64 `json:"miss_p99_us"`
	MissMaxUS float64 `json:"miss_max_us"`
}

// MoveWindow counts requests that overlapped a move.
type MoveWindow struct {
	Requests  int     `json:"requests"`
	Failed    int     `json:"failed"`
	ErrorRate float64 `json:"error_rate"`
}

func locatorCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f locatorFlags
	fs := flag.NewFlagSet("locator", flag.ContinueOnError)
	fs.StringVar(&f.locator, "locator", "http://127.0.0.1:7080", "controld base URL (resolve and admin API)")
	fs.StringVar(&f.tenant, "tenant", "fb", "tenant of the databases fb-0..fb-N-1")
	fs.StringVar(&f.scheme, "scheme", "http", "scheme for agent endpoints given as host:port (https for TLS agents)")
	fs.StringVar(&f.ca, "ca", "", "PEM CA to trust for https endpoints")
	fs.StringVar(&f.out, "out", "", "write the summary JSON here")
	fs.IntVar(&f.dbs, "dbs", 16, "databases")
	fs.IntVar(&f.conc, "conc", 64, "concurrent clients (closed loop, one shared client)")
	fs.Uint64Var(&f.seed, "seed", 1, "random seed")
	fs.Float64Var(&f.write, "write", 0.2, "fraction of requests that are inserts; the rest are point reads")
	fs.DurationVar(&f.duration, "duration", 10*time.Second, "measured time")
	fs.DurationVar(&f.warmup, "warmup", time.Second, "unmeasured time first")
	fs.DurationVar(&f.timeout, "timeout", 5*time.Second, "per-call deadline (covers resolves, retries and waits)")
	fs.DurationVar(&f.move, "move-every", 0, "move a random database to another VM this often (0 = never; needs TSDB_ADMIN_TOKEN)")
	fs.BoolVar(&f.setup, "setup", false, "create the databases and their table first (needs TSDB_ADMIN_TOKEN)")
	fs.StringVar(&f.edge, "edge", "", "send queries to this edge base URL (resolves still go to -locator)")
	fs.Float64Var(&f.rate, "rate", 0, "requests per second across all clients (0 = closed loop)")
	fs.Float64Var(&f.zipf, "zipf", 0, "database popularity: Zipf exponent > 1 (fb-0 hottest); 0 = uniform")
	fs.StringVar(&f.ackLog, "ack-log", "", "append each write's request id and outcome here; inserts store the id (frontbench verify)")
	fs.StringVar(&f.events, "events", "", "append errors, retries and moves here as JSON lines")
	fs.StringVar(&f.runID, "run-id", "", "request id prefix (unique per run when logging acks; default random)")
	if err := parse(fs, args, stderr); err != nil {
		return err
	}
	if f.dbs < 1 || f.conc < 1 || f.write < 0 || f.write > 1 || f.duration <= 0 || f.warmup < 0 || f.move < 0 || f.rate < 0 {
		fmt.Fprintln(stderr, "frontbench locator: need dbs, conc >= 1, 0 <= write <= 1, duration > 0")
		return usageError{errors.New("bad flags")}
	}
	hc, err := tlsx.HTTPClient(f.ca) // controld's admin API, over TLS from the same private CA
	if err != nil {
		return errs.Wrap(module, err, "cannot load the CA file %s", f.ca)
	}
	hc.Timeout = time.Minute
	ad := &adminAPI{base: strings.TrimSuffix(f.locator, "/"), token: os.Getenv("TSDB_ADMIN_TOKEN"), http: hc}
	if (f.setup || f.move > 0) && ad.token == "" {
		fmt.Fprintln(stderr, "frontbench locator: -setup and -move-every need TSDB_ADMIN_TOKEN")
		return usageError{errors.New("missing TSDB_ADMIN_TOKEN")}
	}
	apiKey := os.Getenv("TSDB_API_KEY")
	if apiKey == "" && f.setup {
		var err error
		if apiKey, err = ad.createKey(ctx, f.tenant); err != nil {
			return err
		}
	}
	if apiKey == "" {
		fmt.Fprintln(stderr, "frontbench locator: set TSDB_API_KEY (or use -setup with TSDB_ADMIN_TOKEN)")
		return usageError{errors.New("missing TSDB_API_KEY")}
	}
	if f.setup {
		if err := setupDBs(ctx, f, ad, apiKey); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "set up %d databases for tenant %s\n", f.dbs, f.tenant)
	}
	sum, err := driveLocator(ctx, f, ad, apiKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "locator conc=%d write=%.2f dbs=%d: %d req, %.0f ok/s, p50 %.0f µs, p99 %.0f µs, errors %.4f; "+
		"resolve hits %d (p50 %.1f µs) misses %d (p50 %.0f µs, p99 %.0f µs); reroutes %d, retries %d; moves %d (failed %d), "+
		"during moves %d req, error rate %.4f; codes %v\n",
		sum.Conc, sum.WriteFrac, sum.DBs, sum.Requests, sum.OKPerSec, sum.P50us, sum.P99us, sum.ErrorRate,
		sum.Resolve.Hits, sum.Resolve.HitP50us, sum.Resolve.Misses, sum.Resolve.MissP50us, sum.Resolve.MissP99us,
		sum.Reroutes, sum.Retries, sum.Moves, sum.MoveFailed, sum.DuringMove.Requests, sum.DuringMove.ErrorRate, sum.Codes)
	return finish(f.out, sum, sum.Codes)
}

func fbName(i int) string { return fmt.Sprintf("fb-%d", i) }

// clientConfig is the client every locator call uses; callers add their own fields.
func (f locatorFlags) clientConfig(apiKey string) client.Config {
	return client.Config{Locator: f.locator, APIKey: apiKey, Tenant: f.tenant, Scheme: f.scheme, CAFile: f.ca, Edge: f.edge}
}

// adminAPI is the slice of controld's admin API the bench uses.
type adminAPI struct {
	base, token string
	http        *http.Client
}

// call sends one admin request and decodes the answer into out (if not nil); it returns the HTTP status.
func (a *adminAPI) call(ctx context.Context, path string, body, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, errs.Wrap(module, err, "bad controld URL %s", a.base)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	res, err := a.http.Do(req)
	if err != nil {
		return 0, errs.Wrap(module, err, "controld %s: no answer", path)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	return res.StatusCode, nil
}

func (a *adminAPI) createKey(ctx context.Context, tenant string) (string, error) {
	var out struct{ Key, Message string }
	code, err := a.call(ctx, "/v1/tenants/"+url.PathEscape(tenant)+"/api-keys", map[string]string{"principal": "frontbench"}, &out)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK || out.Key == "" {
		return "", errs.Wrap(module, nil, "cannot create an api key for tenant %s: HTTP %d %s", tenant, code, out.Message)
	}
	return out.Key, nil
}

// setupDBs creates the databases (an existing one is fine) and their table, through the client.
func setupDBs(ctx context.Context, f locatorFlags, ad *adminAPI, apiKey string) error {
	cfg := f.clientConfig(apiKey)
	cfg.Timeout, cfg.MaxReroutes = 30*time.Second, 20
	c, err := client.New(cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	for i := range f.dbs {
		var out struct{ Code, Message string }
		code, err := ad.call(ctx, "/v1/databases", map[string]string{"tenant": f.tenant, "name": fbName(i)}, &out)
		if err != nil {
			return err
		}
		if code != http.StatusOK && out.Code != "conflict" {
			return errs.Wrap(module, nil, "cannot create database %s:%s: HTTP %d %s %s", f.tenant, fbName(i), code, out.Code, out.Message)
		}
		if _, err := c.Do(ctx, client.Request{DB: fbName(i), Kind: front.KindWrite,
			SQL: "create table if not exists fb (id integer primary key, v blob)"}); err != nil {
			return errs.Wrap(module, err, "cannot create table fb in %s", fbName(i))
		}
	}
	return nil
}

// latencies collects resolve lookups from the client's hook (shared by every worker). Slices are preallocated
// so that growing them never stalls the workers inside a measured call.
type latencies struct {
	on        atomic.Bool
	mu        sync.Mutex
	hit, miss []time.Duration
	reroutes  int
}

const maxSamples = 2_000_000

func (l *latencies) resolve(e client.ResolveEvent) {
	if !l.on.Load() || e.Err != nil {
		return
	}
	l.mu.Lock()
	if e.Hit && len(l.hit) < maxSamples {
		l.hit = append(l.hit, e.Wait)
	} else if !e.Hit && len(l.miss) < maxSamples {
		l.miss = append(l.miss, e.Wait)
	}
	l.mu.Unlock()
}

func (l *latencies) retry(e client.RetryEvent) {
	if e.Reroute && l.on.Load() {
		l.mu.Lock()
		l.reroutes++
		l.mu.Unlock()
	}
}

// driveLocator runs conc closed-loop workers on one client, plus the mover, for warmup + duration.
func driveLocator(ctx context.Context, f locatorFlags, ad *adminAPI, apiKey string) (sum LocatorSummary, err error) {
	lat := &latencies{hit: make([]time.Duration, 0, min(maxSamples, f.conc*int(f.duration/time.Millisecond)))}
	budget := &retry.Budget{Ratio: client.DefaultBudgetRatio}
	slog, err := openSoakLog(f.ackLog, f.events)
	if err != nil {
		return LocatorSummary{}, err
	}
	defer func() {
		if cerr := slog.Close(); cerr != nil && err == nil {
			err = errs.Wrap(module, cerr, "cannot write the soak logs")
		}
	}()
	if f.runID == "" {
		f.runID = fmt.Sprintf("r%x", rand.Uint64())
	}
	pace := newPacer(f.rate)
	cfg := f.clientConfig(apiKey)
	cfg.Timeout, cfg.Budget = f.timeout, budget
	cfg.Hooks = client.Hooks{Resolve: lat.resolve, Retry: func(e client.RetryEvent) { lat.retry(e); slog.retry(e) }}
	c, err := client.New(cfg)
	if err != nil {
		return LocatorSummary{}, err
	}
	defer c.Close()
	names := make([]string, f.dbs)
	for i := range names {
		names[i] = fbName(i)
	}
	start := time.Now()
	measureFrom, end := start.Add(f.warmup), start.Add(f.warmup+f.duration)
	var movingUntil atomic.Int64 // unix ns: requests starting before this overlap a move

	type result struct {
		lat                    []time.Duration
		codes                  map[string]int
		moveReqs, moveFailures int
	}
	results := make([]result, f.conc)
	var retries0 int64
	var wg sync.WaitGroup
	wg.Go(func() { // turns measurement on at measureFrom
		select {
		case <-time.After(time.Until(measureFrom)):
		case <-ctx.Done():
		}
		_, retries0 = budget.Counts()
		lat.on.Store(true)
	})
	var moves, moveFailed int
	var moveDur []time.Duration
	if f.move > 0 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(f.seed, 0))
			tick := time.NewTicker(f.move)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				if time.Now().Add(moveWindow).After(end) {
					return
				}
				key := f.tenant + ":" + names[r.IntN(f.dbs)]
				movingUntil.Store(time.Now().Add(time.Hour).UnixNano())
				t0 := time.Now()
				mctx, cancel := context.WithTimeout(ctx, time.Minute)
				code, err := ad.call(mctx, "/v1/databases/"+url.PathEscape(key)+"/move", map[string]string{}, nil)
				cancel()
				slog.event("move", map[string]any{"db": key, "status": code, "ok": err == nil && code == http.StatusOK, "took_ms": time.Since(t0).Milliseconds()})
				movingUntil.Store(time.Now().Add(moveWindow).UnixNano())
				switch {
				case t0.Before(measureFrom):
				case err != nil || code != http.StatusOK:
					moveFailed++
				default:
					moves++
					moveDur = append(moveDur, time.Since(t0))
				}
			}
		})
	}
	for w := range f.conc {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(f.seed, uint64(w)+1))
			pick := picker(r, f.dbs, f.zipf)
			res := result{codes: map[string]int{}}
			blob := make([]byte, 64)
			for seq := 0; ctx.Err() == nil; seq++ {
				pace.wait(ctx)
				db := names[pick()]
				var req client.Request
				switch {
				case r.Float64() >= f.write:
				case slog.ackOn: // the row's v is the request id, so verify can find it
					rid := fmt.Sprintf("%s-%d-%d", f.runID, w, seq)
					req = client.Request{DB: db, Kind: front.KindWrite, SQL: "insert into fb (v) values (?)", Args: []any{rid}, RequestID: rid}
				default:
					for i := 0; i < len(blob); i += 8 {
						binary.LittleEndian.PutUint64(blob[i:], r.Uint64())
					}
					req = client.Request{DB: db, Kind: front.KindWrite, SQL: "insert into fb (v) values (?)", Args: []any{blob}}
				}
				if req.Kind == "" {
					req = client.Request{DB: db, Kind: front.KindRead, SQL: "select id, v from fb where id = ?", Args: []any{r.Int64N(1000) + 1}}
				}
				t0 := time.Now()
				if t0.After(end) {
					break
				}
				_, err := c.Do(ctx, req)
				if ctx.Err() != nil {
					break // interrupted: the outcome is the run's, not the system's
				}
				if req.RequestID != "" {
					slog.write(db, req.RequestID, err)
				}
				slog.callError(db, req.Kind, time.Since(t0), err)
				if !t0.After(measureFrom) {
					continue
				}
				res.lat = append(res.lat, time.Since(t0))
				res.codes[client.CodeOf(err)]++
				if t0.UnixNano() < movingUntil.Load() {
					res.moveReqs++
					if err != nil {
						res.moveFailures++
					}
				}
			}
			results[w] = res
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return LocatorSummary{}, errs.Wrap(module, ctx.Err(), "run interrupted; no summary")
	}
	_, retries1 := budget.Counts()

	var all []time.Duration
	codes := map[string]int{}
	s := LocatorSummary{Moves: moves, MoveFailed: moveFailed, Retries: retries1 - retries0}
	for _, r := range results {
		all = append(all, r.lat...)
		for c, n := range r.codes {
			codes[c] += n
		}
		s.DuringMove.Requests += r.moveReqs
		s.DuringMove.Failed += r.moveFailures
	}
	s.Summary = summarize("locator", f.dbs, f.conc, f.write, f.duration, start, all, codes)
	if s.Requests > 0 {
		s.ErrorRate = float64(s.Requests-codes["ok"]) / float64(s.Requests)
	}
	if s.DuringMove.Requests > 0 {
		s.DuringMove.ErrorRate = float64(s.DuringMove.Failed) / float64(s.DuringMove.Requests)
	}
	slices.Sort(lat.hit)
	slices.Sort(lat.miss)
	s.Resolve = ResolveStats{Hits: len(lat.hit), Misses: len(lat.miss), HitP50us: quantileUS(lat.hit, 0.5), HitP99us: quantileUS(lat.hit, 0.99),
		MissP50us: quantileUS(lat.miss, 0.5), MissP99us: quantileUS(lat.miss, 0.99), MissMaxUS: quantileUS(lat.miss, 1)}
	s.Reroutes = lat.reroutes
	slices.Sort(moveDur)
	s.MoveP50ms = quantileUS(moveDur, 0.5) / 1e3
	return s, nil
}
