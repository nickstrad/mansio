package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/schema"
	"github.com/nickstrad/ts-db-go/internal/ticket"
	"github.com/nickstrad/ts-db-go/internal/tlsx"
	"github.com/nickstrad/ts-db-go/internal/workload"
)

type runFlags struct {
	mode, dir, url, proto, key, vm, out, ca string
	dbs, tenants, conc                      int
	seed                                    uint64
	write                                   float64
	duration, warmup, timeout               time.Duration
}

// Summary is one run's result (also written as JSON with -out).
type Summary struct {
	Mode       string         `json:"mode"`
	Proto      string         `json:"proto,omitempty"`
	DBs        int            `json:"dbs"`
	Conc       int            `json:"conc"`
	WriteFrac  float64        `json:"write_frac"`
	Duration   string         `json:"duration"`
	Requests   int            `json:"requests"`
	Codes      map[string]int `json:"codes"`
	OKPerSec   float64        `json:"ok_per_sec"`
	P50us      float64        `json:"p50_us"`
	P90us      float64        `json:"p90_us"`
	P99us      float64        `json:"p99_us"`
	P999us     float64        `json:"p999_us"`
	MaxUS      float64        `json:"max_us"`
	GoVersion  string         `json:"go"`
	CPUs       int            `json:"cpus"`
	StartedUTC string         `json:"started_utc"`
}

func runCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var f runFlags
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&f.mode, "mode", "http", "inproc | pipeline | http")
	fs.StringVar(&f.dir, "dir", "", "database directory (inproc, pipeline)")
	fs.StringVar(&f.url, "url", "http://127.0.0.1:7070", "agentd base URL (http)")
	fs.StringVar(&f.proto, "proto", "h1", "h1 | h2c (plain http) | h2 (https)")
	fs.StringVar(&f.ca, "ca", "", "PEM CA to trust for an https -url (a private CA, e.g. the edge's)")
	fs.StringVar(&f.key, "key", "", "issuer key from keygen (http; pipeline makes its own if empty)")
	fs.StringVar(&f.vm, "vm", "vm-local", "ticket audience")
	fs.StringVar(&f.out, "out", "", "write the summary JSON here")
	fs.IntVar(&f.dbs, "dbs", 16, "databases db-0..db-N-1 (as seeded)")
	fs.IntVar(&f.tenants, "tenants", 4, "tenants the databases are spread over (db i → tenant i mod T)")
	fs.IntVar(&f.conc, "conc", 64, "concurrent clients (closed loop)")
	fs.Uint64Var(&f.seed, "seed", 1, "schema seed used by seed")
	fs.Float64Var(&f.write, "write", 0, "fraction of requests that are inserts; the rest are point reads")
	fs.DurationVar(&f.duration, "duration", 10*time.Second, "measured time")
	fs.DurationVar(&f.warmup, "warmup", time.Second, "unmeasured time first")
	fs.DurationVar(&f.timeout, "timeout", 2*time.Second, "per-request deadline")
	if err := parse(fs, args, stderr); err != nil {
		return err
	}
	if f.dbs < 1 || f.conc < 1 || f.tenants < 1 || f.write < 0 || f.write > 1 || f.duration <= 0 || f.warmup < 0 {
		fmt.Fprintln(stderr, "frontbench run: need dbs, conc, tenants >= 1, 0 <= write <= 1, duration > 0")
		return usageError{errors.New("bad flags")}
	}
	t, closeT, err := newTarget(f)
	if err != nil {
		return err
	}
	defer closeT()
	sum, err := drive(ctx, f, t)
	if err != nil {
		return err
	}
	mode := sum.Mode
	if sum.Proto != "" {
		mode += "/" + sum.Proto
	}
	fmt.Fprintf(stdout, "%s conc=%d write=%.2f dbs=%d: %d req, %.0f ok/s, p50 %.0f µs, p99 %.0f µs, p99.9 %.0f µs, codes %v\n",
		mode, sum.Conc, sum.WriteFrac, sum.DBs, sum.Requests,
		sum.OKPerSec, sum.P50us, sum.P99us, sum.P999us, sum.Codes)
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

// target sends one statement to database db and returns its wire code.
type target func(ctx context.Context, db int, st workload.Statement) string

func newTarget(f runFlags) (target, func(), error) {
	switch f.mode {
	case "inproc":
		host, err := openActors(f)
		if err != nil {
			return nil, nil, err
		}
		return inproc(host, f), host.Close, nil
	case "pipeline":
		host, err := openActors(f)
		if err != nil {
			return nil, nil, err
		}
		kid, priv, err := issuer(f)
		if err != nil {
			host.Close()
			return nil, nil, err
		}
		keys := ticket.NewKeyRing(map[string]ed25519.PublicKey{kid: priv.Public().(ed25519.PublicKey)})
		epochs := front.StaticAdmitter{}
		for i := range f.dbs {
			epochs[dbID(i)] = 1
		}
		srv := front.NewServer(front.Config{DefaultTimeout: f.timeout, MaxTimeout: f.timeout, StmtTimeout: f.timeout},
			ticket.NewCache(ticket.Verifier{Keys: keys, VM: f.vm, Skew: 5 * time.Second}, 1<<16), epochs,
			front.ActorExecutor{Host: host}, nil)
		toks, err := tickets(f, kid, priv)
		if err != nil {
			host.Close()
			return nil, nil, err
		}
		return func(ctx context.Context, db int, st workload.Statement) string {
			out := srv.Handle(ctx, front.Input{Ticket: toks[db], Body: requestBody(db, st)})
			return string(out.Resp.Code)
		}, host.Close, nil
	case "http":
		kid, priv, err := issuer(f)
		if err != nil {
			return nil, nil, err
		}
		toks, err := tickets(f, kid, priv)
		if err != nil {
			return nil, nil, err
		}
		return httpTarget(f, toks)
	}
	return nil, nil, usageError{fmt.Errorf("unknown mode %q", f.mode)}
}

func issuer(f runFlags) (string, ed25519.PrivateKey, error) {
	if f.key == "" {
		if f.mode == "http" {
			return "", nil, usageError{errors.New("-key is required for -mode http")}
		}
		_, priv, _ := ed25519.GenerateKey(nil)
		return "bench", priv, nil
	}
	return loadIssuer(f.key)
}

// tickets mints one read|write ticket per database, valid for the whole run.
func tickets(f runFlags, kid string, priv ed25519.PrivateKey) ([]string, error) {
	now := time.Now()
	out := make([]string, f.dbs)
	for i := range f.dbs {
		tok, err := ticket.Mint(priv, ticket.Claims{KID: kid, TID: fmt.Sprintf("t-%d", i%f.tenants), DBID: dbID(i), VM: f.vm,
			Epoch: 1, Gen: 1, Sub: "frontbench", Perms: ticket.Read | ticket.Write, IAT: now.Unix(),
			EXP: now.Add(f.warmup + f.duration + 5*time.Minute).Unix()})
		if err != nil {
			return nil, errs.Wrap(module, err, "cannot mint tickets (check -vm)")
		}
		out[i] = tok
	}
	return out, nil
}

func openActors(f runFlags) (*front.StaticHost, error) {
	if f.dir == "" {
		return nil, usageError{errors.New("-dir is required for this mode")}
	}
	actors := map[string]*actor.Actor{}
	for i := range f.dbs {
		path := filepath.Join(f.dir, dbID(i)+".db")
		if _, err := os.Stat(path); err != nil {
			front.NewStaticHost(actors).Close()
			return nil, errs.Wrap(module, err, "database %s missing; run frontbench seed first", dbID(i))
		}
		a := actor.New(i, path, actor.Config{})
		if err := a.Start(true); err != nil {
			front.NewStaticHost(actors).Close()
			return nil, err
		}
		actors[dbID(i)] = a
	}
	return front.NewStaticHost(actors), nil
}

// inproc is the baseline: what actor.Do does (Submit, Wait), with the same deadline, statement timeout and row
// collection the front asks for.
func inproc(host *front.StaticHost, f runFlags) target {
	return func(ctx context.Context, db int, st workload.Statement) string {
		a, _ := host.Lookup(ctx, dbID(db))
		r := &actor.Request{SQL: st.SQL, Args: st.Args, Write: st.Write, StmtTimeout: f.timeout, Deadline: time.Now().Add(f.timeout)}
		if !st.Write {
			r.MaxRows = 10_000
		}
		if err := a.Submit(r); err != nil {
			return "shed"
		}
		if res := r.Wait(ctx); res.Err != nil {
			return "error"
		}
		return "ok"
	}
}

// requestSeq makes request ids unique per run: the actor applies each id at most once per database (CP-2), so
// a shared id would turn every write after the first into a duplicate.
var (
	requestRun = time.Now().UnixNano()
	requestSeq atomic.Int64
)

func requestBody(db int, st workload.Statement) []byte {
	args := make([]any, len(st.Args))
	for i, a := range st.Args {
		if b, ok := a.([]byte); ok {
			a = front.Blob{B64: base64.StdEncoding.EncodeToString(b)}
		}
		args[i] = a
	}
	kind := front.KindRead
	if st.Write {
		kind = front.KindWrite
	}
	b, _ := json.Marshal(map[string]any{"db": dbID(db), "request_id": fmt.Sprintf("fb-%d-%d", requestRun, requestSeq.Add(1)), "kind": kind, "sql": st.SQL, "args": args})
	return b
}

func httpTarget(f runFlags, toks []string) (target, func(), error) {
	tr := &http.Transport{MaxIdleConns: f.conc * 2, MaxIdleConnsPerHost: f.conc * 2, IdleConnTimeout: time.Minute}
	switch f.proto {
	case "h2c":
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetUnencryptedHTTP2(true)
	case "h2":
		tr.ForceAttemptHTTP2 = true
	}
	if f.ca != "" {
		pool, err := tlsx.LoadPool(f.ca)
		if err != nil {
			return nil, nil, errs.Wrap(module, err, "cannot load -ca").With("path", f.ca)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	timeoutMS := strconv.FormatInt(f.timeout.Milliseconds(), 10)
	client := &http.Client{Transport: tr}
	url := f.url + front.QueryPath
	auth := make([]string, len(toks))
	for i, t := range toks {
		auth[i] = "Bearer " + t
	}
	return func(ctx context.Context, db int, st workload.Statement) string {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(requestBody(db, st)))
		req.Header.Set("Authorization", auth[db])
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(front.TimeoutHeader, timeoutMS)
		res, err := client.Do(req)
		if err != nil {
			return "transport_error"
		}
		defer res.Body.Close()
		var out struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return "bad_body"
		}
		io.Copy(io.Discard, res.Body)
		return out.Code
	}, tr.CloseIdleConnections, nil
}

// drive runs conc closed-loop clients for warmup + duration and summarizes the measured part.
func drive(ctx context.Context, f runFlags, send target) (Summary, error) {
	cat := schema.NewCatalog(f.seed, schema.DefaultOptions())
	cat.Ensure(f.dbs)
	type result struct {
		lat   []time.Duration
		codes map[string]int
	}
	results := make([]result, f.conc)
	start := time.Now()
	measureFrom, end := start.Add(f.warmup), start.Add(f.warmup+f.duration)
	var wg sync.WaitGroup
	for w := range f.conc {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(f.seed, uint64(w)+1))
			gens := map[int]*workload.Generator{}
			res := result{codes: map[string]int{}}
			for ctx.Err() == nil {
				db := r.IntN(f.dbs)
				g := gens[db]
				if g == nil {
					g = workload.NewGenerator(cat.Get(db), workload.DefaultOptions())
					gens[db] = g
				}
				class := workload.PointRead
				if r.Float64() < f.write {
					class = workload.Insert
				}
				st := g.Next(r, class)
				t0 := time.Now()
				if t0.After(end) {
					break
				}
				code := send(ctx, db, st)
				if t0.After(measureFrom) {
					res.lat = append(res.lat, time.Since(t0))
					res.codes[code]++
				}
			}
			results[w] = res
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return Summary{}, errs.Wrap(module, ctx.Err(), "run interrupted; no summary")
	}
	var lat []time.Duration
	codes := map[string]int{}
	for _, r := range results {
		lat = append(lat, r.lat...)
		for c, n := range r.codes {
			codes[c] += n
		}
	}
	slices.Sort(lat)
	q := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		return float64(lat[min(len(lat)-1, int(p*float64(len(lat))))].Nanoseconds()) / 1e3
	}
	s := Summary{Mode: f.mode, DBs: f.dbs, Conc: f.conc, WriteFrac: f.write, Duration: f.duration.String(), Requests: len(lat),
		Codes: codes, OKPerSec: float64(codes["ok"]) / f.duration.Seconds(), P50us: q(0.5), P90us: q(0.9), P99us: q(0.99),
		P999us: q(0.999), MaxUS: q(1), GoVersion: runtime.Version(), CPUs: runtime.NumCPU(), StartedUTC: start.UTC().Format(time.RFC3339)}
	if f.mode == "http" {
		s.Proto = f.proto
	}
	return s, nil
}
