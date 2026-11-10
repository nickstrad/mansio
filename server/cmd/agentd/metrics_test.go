package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/front/fronttest"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

// sample returns the value of the series whose line starts with series (name plus its exact label set).
func sample(t *testing.T, text, series string) float64 {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no series %s in:\n%s", series, text)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// RI-11 acceptance, agent side: a scripted fault sequence against a running agent, then one scrape of
// /metrics on the lifecycle listener shows every guide-O1 counter for the front, admission and actors.
func TestMetricCatalogAfterFaultScript(t *testing.T) {
	cfg := fixture(t, []string{"db-1"}, `,"health_listen":"127.0.0.1:0","ready_interval":"50ms","ready_timeout":"30ms","statement_timeout":"5s"`)
	os.WriteFile(filepath.Join(filepath.Dir(cfg), "dbs", "db-bad.db"), []byte(strings.Repeat("not a database ", 512)), 0o644)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	life := lifecycleAddr(t, addrFile)
	waitReady(t, life)

	send := func(db string, epoch uint64, sql string, timeoutMS string, auth bool) {
		b, _ := json.Marshal(map[string]any{"db": db, "kind": "read", "sql": sql})
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.QueryPath, bytes.NewReader(b))
		if auth {
			c := iss.Claims(db, "vm-1", time.Now(), time.Minute)
			c.Epoch = epoch
			req.Header.Set("Authorization", "Bearer "+iss.Mint(t, c))
		}
		if timeoutMS != "" {
			req.Header.Set(front.TimeoutHeader, timeoutMS)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	send("db-1", 1, fronttest.ScanSQL, "", true)   // ok
	send("db-1", 1, fronttest.ScanSQL, "", true)   // ok
	send("db-1", 2, fronttest.ScanSQL, "", true)   // wrong_owner: app_error
	send("db-1", 1, fronttest.BadSQL, "", true)    // bad_request: app_error
	send("db-1", 1, fronttest.ScanSQL, "", false)  // ticket_invalid: app_error
	send("db-bad", 1, fronttest.ScanSQL, "", true) // unavailable (not open): refused
	send("db-1", 1, fronttest.SlowSQL, "50", true) // expired: timeout
	res, err := http.Get("http://" + life + metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	text := string(body)
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("content type %q", res.Header.Get("Content-Type"))
	}
	for series, want := range map[string]float64{
		`mansio_requests_total{point="front",class="ok",code="ok"}`:                    2,
		`mansio_requests_total{point="front",class="app_error",code="wrong_owner"}`:    1,
		`mansio_requests_total{point="front",class="app_error",code="bad_request"}`:    1,
		`mansio_requests_total{point="front",class="app_error",code="ticket_invalid"}`: 1,
		`mansio_requests_total{point="front",class="refused",code="unavailable"}`:      1,
		`mansio_requests_total{point="front",class="app_error",code="stmt_timeout"}`:   1,
		`mansio_requests_total{point="front",class="timeout",code="expired"}`:          0, // counted: TestServerStatsCountEveryAnswer
		`mansio_requests_total{point="front",class="ambiguous",code="expired"}`:        0,
		`mansio_requests_total{point="front",class="5xx",code="internal"}`:             0,
		`mansio_request_latency_seconds_count{point="front"}`:                          7,
		`mansio_in_flight{point="front"}`:                                              0,
		`mansio_startup_pending`:                                                       1, // db-bad
		`mansio_ready`:                                                                 1,
		`mansio_admission_shed_total`:                                                  0,
		// B1: per-queue capacity, refusals and waits; db-1's mailbox is the default 256.
		`mansio_queue_capacity{queue="mailbox"}`:                       256,
		`mansio_queue_capacity{queue="read"}`:                          256,
		`mansio_queue_rejected_total{queue="admission",reason="full"}`: 0,
		`mansio_queue_rejected_total{queue="mailbox",reason="full"}`:   0,
		`mansio_queue_rejected_total{queue="read",reason="full"}`:      0,
	} {
		if got := sample(t, text, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	for _, series := range []string{`mansio_in_flight_high_water{point="front"}`, `mansio_admission{stat="in_flight_high_water"}`,
		`mansio_actors`, `mansio_startup_pending_age_seconds`, `mansio_queue_capacity{queue="admission"}`,
		`mansio_queue_wait_seconds_count{queue="admission"}`, `mansio_queue_wait_seconds_count{queue="mailbox"}`} {
		if sample(t, text, series) < 1e-9 {
			t.Errorf("%s is 0", series)
		}
	}
	for _, series := range []string{`mansio_actor_queue_depth{stat="p99"}`, `mansio_admission{stat="queued"}`, `mansio_admission_drain_per_second`,
		`mansio_request_latency_seconds_bucket{point="front",le="+Inf"}`} {
		sample(t, text, series)
	}
	cancel()
	<-done
}

// RI-11 acceptance, dependency side (wired at CP-1): breaker, S3, virtual-actor host and ownership adapters,
// each driven through a fault, then scraped; their transitions log stable message codes.
func TestDependencyMetricsAndCodes(t *testing.T) {
	var logb syncBuf
	log := slog.New(slog.NewJSONHandler(&logb, nil))
	reg := metrics.NewRegistry()

	// Breaker: trip it, then let it half-open.
	b, err := breaker.New(breaker.Config{MinCalls: 1, CoolDown: time.Hour, OnChange: logx.BreakerLog(log, "s3")})
	if err != nil {
		t.Fatal(err)
	}
	metrics.Breakers(reg, map[string]*breaker.Breaker{"s3": b})
	p, _, _ := b.Allow(time.Now())
	b.Done(p, time.Now(), breaker.Failure)

	// S3: a snapshot as objstore reports it after a throttled GET and a retry.
	s3Metrics(reg, func() map[string]objstore.OpStats {
		lat := make([]int64, len(objstore.LatencyBuckets)+1)
		lat[3] = 2
		return map[string]objstore.OpStats{objstore.OpFetch: {ByKind: map[objstore.Kind]int64{objstore.OK: 1, objstore.Transient: 1},
			Retried: 1, Bytes: 4096, Latency: lat}}
	})

	// Virtual-actor host: one write-back fails (store down), one tick times out.
	s := durabletest.NewStore()
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: s,
		CreateMissing: true, WriteBackTimeout: 20 * time.Millisecond, Policy: vactor.Policy{WriteBackEvery: time.Nanosecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	res, err := h.Get(context.Background(), "t/a")
	if err != nil {
		t.Fatal(err)
	}
	res.DB.Query(context.Background(), vactortest.Put("v1"))
	ticks := &tickLog{log: log}
	vactorMetrics(reg, h, ticks.lastTick)
	s.SetFaults(durabletest.Faults{Delay: time.Second})
	time.Sleep(time.Millisecond)
	r, err := h.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ticks.report(r)
	s.SetFaults(durabletest.Faults{})

	// Ownership: claim, then a higher epoch supersedes it at reconcile after a restart.
	oe := &ownerEvents{log: log}
	oe.register(reg)
	pl, objs, store := ownertest.NewPlacement(), ownertest.NewObjects(), ownertest.NewMemStore()
	wb := &ownertest.WriteBack{Objects: objs, Disk: ownertest.NewDisk()}
	tab, err := owner.Open(context.Background(), owner.Config{Self: "a", Store: store, Placement: pl, WriteBack: wb, OnChange: oe.onChange})
	if err != nil {
		t.Fatal(err)
	}
	pl.Create("k", owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive})
	if _, err := tab.Claim(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	tab, _ = owner.Open(context.Background(), owner.Config{Self: "a", Store: store, Placement: pl, WriteBack: wb, OnChange: oe.onChange})
	pl.Create("k", owner.Place{Owner: "b", Epoch: 2, State: owner.PlaceActive})
	rec := reconcileLog(log)
	rec(tab.ReconcileAll(context.Background(), owner.ReconcileOptions{}))

	text := scrape(t, reg)
	for series, want := range map[string]float64{
		`mansio_breaker_state{dep="s3",state="open"}`:                                     1,
		`mansio_breaker_state{dep="s3",state="closed"}`:                                   0,
		`mansio_breaker_transitions_total{dep="s3"}`:                                      1,
		`mansio_s3_requests_total{point="s3",op="fetch",class="ok",kind="ok"}`:            1,
		`mansio_s3_requests_total{point="s3",op="fetch",class="5xx",kind="transient"}`:    1,
		`mansio_s3_requests_total{point="s3",op="fetch",class="timeout",kind="canceled"}`: 0,
		`mansio_s3_retries_total{op="fetch"}`:                                             1,
		`mansio_s3_bytes_total{op="fetch"}`:                                               4096,
		`mansio_s3_latency_seconds_count{point="s3",op="fetch"}`:                          2,
		`mansio_vactor_events_total{event="activation"}`:                                  1,
		`mansio_vactor_events_total{event="writeback_failure"}`:                           1,
		`mansio_vactor{stat="active"}`:                                                    1,
		`mansio_vactor_last_writeback_ok_timestamp_seconds`:                               0,
		`mansio_vactor_tick{stat="timed_out"}`:                                            1,
		`mansio_vactor_dirty`:                                                             1,
		`mansio_queue_capacity{queue="fetch"}`:                                            256,
		`mansio_queue_rejected_total{queue="fetch",reason="full"}`:                        0,
		`mansio_queue_rejected_total{queue="fetch",reason="failure_backoff"}`:             0,
		`mansio_owner_transitions_total{to="active"}`:                                     1,
		`mansio_owner_transitions_total{to="fenced"}`:                                     1,
	} {
		if got := sample(t, text, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if sample(t, text, `mansio_vactor_oldest_dirty_age_seconds`) <= 0 {
		t.Error("oldest dirty age is 0 with a dirty database")
	}
	for _, code := range []string{"S3.BREAKER_OPEN", "VACTOR.TICK_SLOW", "OWNER.CLAIMED", "OWNER.SUPERSEDED", "OWNER.RECONCILE_DONE"} {
		if !strings.Contains(logb.String(), `"event":"`+code+`"`) {
			t.Errorf("log lacks %s:\n%s", code, logb.String())
		}
	}
	b.Allow(time.Now().Add(2 * time.Hour)) // the cool-down is over: half-open
	if !strings.Contains(logb.String(), `"event":"S3.BREAKER_HALF_OPEN"`) {
		t.Errorf("no half-open line:\n%s", logb.String())
	}
}

func TestOwnerCodes(t *testing.T) {
	a := owner.Local{State: owner.Active, Epoch: 1}
	tests := []struct {
		from, to owner.Local
		code     string
		level    slog.Level
	}{
		{owner.Local{}, a, "OWNER.CLAIMED", slog.LevelInfo},
		{owner.Local{State: owner.Closed, Epoch: 1}, a, "OWNER.REOPENED", slog.LevelInfo},
		{a, owner.Local{State: owner.Fencing, Epoch: 1}, "OWNER.FENCING", slog.LevelInfo},
		{a, owner.Local{State: owner.Fenced, Epoch: 1}, "OWNER.FENCED", slog.LevelInfo},
		{a, owner.Local{State: owner.Fenced, Epoch: 1, Superseded: true}, "OWNER.SUPERSEDED", slog.LevelWarn},
		{owner.Local{State: owner.Fenced, Epoch: 1}, owner.Local{}, "OWNER.FORGOTTEN", slog.LevelInfo},
	}
	for _, tc := range tests {
		if code, level := ownerCode(tc.from, tc.to); code != tc.code || level != tc.level {
			t.Errorf("%s → %s: %s %v, want %s %v", tc.from, tc.to, code, level, tc.code, tc.level)
		}
	}
}

// B1's overload scenario: one database capped at one statement in flight and two queued, hit by a burst of
// slow reads. Before B1 the scrape showed only depth gauges and one shed counter; now the same overload reads
// as refusals per queue, a capacity to hold the depth against, and how long the admitted requests waited.
func TestOverloadIsVisiblePerQueue(t *testing.T) {
	cfg := fixture(t, []string{"db-1"}, `,"health_listen":"127.0.0.1:0","statement_timeout":"200ms",`+
		`"admission":{"db_in_flight":1,"db_queue":2,"tenant_in_flight":1}`)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	life := lifecycleAddr(t, addrFile)
	waitReady(t, life)

	const burst = 12
	codes := make(chan int, burst)
	for range burst {
		go func() {
			b, _ := json.Marshal(map[string]any{"db": "db-1", "kind": "read", "sql": fronttest.SlowSQL})
			req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.QueryPath, bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+iss.Mint(t, iss.Claims("db-1", "vm-1", time.Now(), time.Minute)))
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			codes <- res.StatusCode
		}()
	}
	var shed int
	for range burst {
		if <-codes == http.StatusServiceUnavailable {
			shed++
		}
	}
	res, err := http.Get("http://" + life + metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	text := string(body)
	if shed < burst-3 {
		t.Fatalf("%d of %d shed; at most 3 fit the database's slot and queue", shed, burst)
	}
	if got := sample(t, text, `mansio_queue_rejected_total{queue="admission",reason="full"}`); got != float64(shed) {
		t.Errorf("admission refusals %v, want the %d shed answers", got, shed)
	}
	// The two that queued behind a 200 ms statement waited: the histogram's sum shows it, its count every
	// admitted request.
	if sum := sample(t, text, `mansio_queue_wait_seconds_sum{queue="admission"}`); sum < 0.1 {
		t.Errorf("admission wait sum %v s, want the queued requests' wait (>= 0.1 s)", sum)
	}
	if n := sample(t, text, `mansio_queue_wait_seconds_count{queue="admission"}`); n < burst-float64(shed) {
		t.Errorf("admission wait count %v, want at least the %d admitted", n, burst-shed)
	}
	if capacity := sample(t, text, `mansio_queue_capacity{queue="admission"}`); capacity <= 0 {
		t.Errorf("admission capacity %v", capacity)
	}
	cancel()
	<-done
}
