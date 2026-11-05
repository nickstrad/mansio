package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/client"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/retry"
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
}

// moveWindow is how long after a move returns its requests still count as "during the move": routes cached
// before the move are found stale only when used.
const moveWindow = 2 * time.Second

// LocatorSummary is one locator run (also written as JSON with -out).
type LocatorSummary struct {
	Mode       string         `json:"mode"` // "locator"
	DBs        int            `json:"dbs"`
	Conc       int            `json:"conc"`
	WriteFrac  float64        `json:"write_frac"`
	Duration   string         `json:"duration"`
	Requests   int            `json:"requests"`
	Codes      map[string]int `json:"codes"` // final code per request (client.CodeOf)
	OKPerSec   float64        `json:"ok_per_sec"`
	ErrorRate  float64        `json:"error_rate"` // non-ok / requests
	P50us      float64        `json:"p50_us"`
	P99us      float64        `json:"p99_us"`
	P999us     float64        `json:"p999_us"`
	MaxUS      float64        `json:"max_us"`
	Resolve    ResolveStats   `json:"resolve"`
	Reroutes   int            `json:"reroutes"`    // re-resolves inside calls
	Retries    int64          `json:"retries"`     // same-route retries (budget spent)
	Moves      int            `json:"moves"`       // moves that succeeded during the measured part
	MoveFailed int            `json:"move_failed"` // moves controld refused or failed
	MoveP50ms  float64        `json:"move_p50_ms"` // move call duration
	DuringMove MoveWindow     `json:"during_move"` // requests started during a move or moveWindow after it
	GoVersion  string         `json:"go"`
	CPUs       int            `json:"cpus"`
	StartedUTC string         `json:"started_utc"`
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
	if err := parse(fs, args, stderr); err != nil {
		return err
	}
	if f.dbs < 1 || f.conc < 1 || f.write < 0 || f.write > 1 || f.duration <= 0 || f.warmup < 0 || f.move < 0 {
		fmt.Fprintln(stderr, "frontbench locator: need dbs, conc >= 1, 0 <= write <= 1, duration > 0")
		return usageError{errors.New("bad flags")}
	}
	ad := &adminAPI{base: strings.TrimSuffix(f.locator, "/"), token: os.Getenv("TSDB_ADMIN_TOKEN"), http: &http.Client{Timeout: time.Minute}}
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
	if f.out != "" {
		b, _ := json.MarshalIndent(sum, "", "  ")
		if err := os.MkdirAll(filepath.Dir(f.out), 0o755); err != nil {
			return errs.Wrap(module, err, "cannot create %s", filepath.Dir(f.out))
		}
		if err := os.WriteFile(f.out, append(b, '\n'), 0o644); err != nil {
			return errs.Wrap(module, err, "cannot write %s", f.out)
		}
	}
	if sum.Codes["ok"] == 0 {
		return errs.Wrap(module, nil, "no request succeeded: codes %v", sum.Codes)
	}
	return nil
}

func fbName(i int) string { return fmt.Sprintf("fb-%d", i) }

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
	c, err := client.New(client.Config{Locator: f.locator, APIKey: apiKey, Tenant: f.tenant, Scheme: f.scheme, CAFile: f.ca,
		Timeout: 30 * time.Second, MaxReroutes: 20})
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

// latencies collects resolve lookups from the client's hook (shared by every worker).
type latencies struct {
	mu        sync.Mutex
	on        bool
	hit, miss []time.Duration
	reroutes  int
}

const maxSamples = 2_000_000

func (l *latencies) resolve(e client.ResolveEvent) {
	l.mu.Lock()
	if l.on && e.Err == nil {
		if e.Hit && len(l.hit) < maxSamples {
			l.hit = append(l.hit, e.Wait)
		} else if !e.Hit && len(l.miss) < maxSamples {
			l.miss = append(l.miss, e.Wait)
		}
	}
	l.mu.Unlock()
}

func (l *latencies) retry(e client.RetryEvent) {
	if e.Reroute {
		l.mu.Lock()
		if l.on {
			l.reroutes++
		}
		l.mu.Unlock()
	}
}

func quantileUS(d []time.Duration, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	return float64(d[min(len(d)-1, int(p*float64(len(d))))].Nanoseconds()) / 1e3
}

// driveLocator runs conc closed-loop workers on one client, plus the mover, for warmup + duration.
func driveLocator(ctx context.Context, f locatorFlags, ad *adminAPI, apiKey string) (LocatorSummary, error) {
	lat := &latencies{}
	budget := &retry.Budget{Ratio: client.DefaultBudgetRatio}
	c, err := client.New(client.Config{Locator: f.locator, APIKey: apiKey, Tenant: f.tenant, Scheme: f.scheme, CAFile: f.ca,
		Timeout: f.timeout, Budget: budget, Hooks: client.Hooks{Resolve: lat.resolve, Retry: lat.retry}})
	if err != nil {
		return LocatorSummary{}, err
	}
	defer c.Close()
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
	measuring := make(chan struct{})
	wg.Go(func() { // turns measurement on at measureFrom
		select {
		case <-time.After(time.Until(measureFrom)):
		case <-ctx.Done():
		}
		lat.mu.Lock()
		lat.on = true
		lat.mu.Unlock()
		_, retries0 = budget.Counts()
		close(measuring)
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
				key := f.tenant + ":" + fbName(r.IntN(f.dbs))
				movingUntil.Store(time.Now().Add(time.Hour).UnixNano())
				t0 := time.Now()
				mctx, cancel := context.WithTimeout(ctx, time.Minute)
				code, err := ad.call(mctx, "/v1/databases/"+url.PathEscape(key)+"/move", map[string]string{}, nil)
				cancel()
				movingUntil.Store(time.Now().Add(moveWindow).UnixNano())
				if t0.Before(measureFrom) {
					continue
				}
				if err != nil || code != http.StatusOK {
					moveFailed++
					continue
				}
				moves++
				moveDur = append(moveDur, time.Since(t0))
			}
		})
	}
	var workers sync.WaitGroup
	for w := range f.conc {
		workers.Go(func() {
			r := rand.New(rand.NewPCG(f.seed, uint64(w)+1))
			res := result{codes: map[string]int{}}
			blob := make([]byte, 64)
			for ctx.Err() == nil {
				db := r.IntN(f.dbs)
				req := client.Request{DB: fbName(db), Kind: front.KindRead, SQL: "select id, v from fb where id = ?", Args: []any{r.Int64N(1000) + 1}}
				if r.Float64() < f.write {
					for i := range blob {
						blob[i] = byte(r.Uint32())
					}
					req = client.Request{DB: fbName(db), Kind: front.KindWrite, SQL: "insert into fb (v) values (?)", Args: []any{blob}}
				}
				t0 := time.Now()
				if t0.After(end) {
					break
				}
				_, err := c.Do(ctx, req)
				if !t0.After(measureFrom) {
					continue
				}
				code := client.CodeOf(err)
				res.lat = append(res.lat, time.Since(t0))
				res.codes[code]++
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
	workers.Wait()
	wg.Wait()
	if ctx.Err() != nil {
		return LocatorSummary{}, errs.Wrap(module, ctx.Err(), "run interrupted; no summary")
	}
	<-measuring
	_, retries1 := budget.Counts()

	var all []time.Duration
	s := LocatorSummary{Mode: "locator", DBs: f.dbs, Conc: f.conc, WriteFrac: f.write, Duration: f.duration.String(),
		Codes: map[string]int{}, Moves: moves, MoveFailed: moveFailed, Retries: retries1 - retries0,
		GoVersion: runtime.Version(), CPUs: runtime.NumCPU(), StartedUTC: start.UTC().Format(time.RFC3339)}
	for _, r := range results {
		all = append(all, r.lat...)
		for c, n := range r.codes {
			s.Codes[c] += n
		}
		s.DuringMove.Requests += r.moveReqs
		s.DuringMove.Failed += r.moveFailures
	}
	slices.Sort(all)
	s.Requests = len(all)
	s.OKPerSec = float64(s.Codes["ok"]) / f.duration.Seconds()
	if s.Requests > 0 {
		s.ErrorRate = float64(s.Requests-s.Codes["ok"]) / float64(s.Requests)
	}
	if s.DuringMove.Requests > 0 {
		s.DuringMove.ErrorRate = float64(s.DuringMove.Failed) / float64(s.DuringMove.Requests)
	}
	s.P50us, s.P99us, s.P999us, s.MaxUS = quantileUS(all, 0.5), quantileUS(all, 0.99), quantileUS(all, 0.999), quantileUS(all, 1)
	lat.mu.Lock()
	slices.Sort(lat.hit)
	slices.Sort(lat.miss)
	s.Resolve = ResolveStats{Hits: len(lat.hit), Misses: len(lat.miss), HitP50us: quantileUS(lat.hit, 0.5), HitP99us: quantileUS(lat.hit, 0.99),
		MissP50us: quantileUS(lat.miss, 0.5), MissP99us: quantileUS(lat.miss, 0.99), MissMaxUS: quantileUS(lat.miss, 1)}
	s.Reroutes = lat.reroutes
	lat.mu.Unlock()
	slices.Sort(moveDur)
	s.MoveP50ms = quantileUS(moveDur, 0.5) / 1e3
	return s, nil
}
