package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/capacity"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/eventlog"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancyhttp"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

var updateCapacity = flag.Bool("update", false, "rewrite agentd's section of docs/capacity.md from the config defaults")

// capacityRows is agentd's part of the capacity table, outermost first, from the config defaults.
func capacityRows() []capacity.Row {
	c := defaults()
	adm, act := c.Admission.WithDefaults(), c.Actor.Actor()
	q := func(queue string) string { return fmt.Sprintf("`mansio_queue_*{queue=%q}`", queue) }
	limit := func(queue string) string {
		return fmt.Sprintf("`mansioctl agent limit %s N` (`POST /admin/limit?queue=%s&n=N`)", queue, queue)
	}
	deadline := fmt.Sprintf("the request's deadline (`default_timeout` %v, at most `max_timeout` %v)", c.DefaultTimeout, c.MaxTimeout)
	return []capacity.Row{
		{Queue: "verify", Protects: "the tenancy store, the schema registry and the placement authority (credential requests on `/v1/ops`)",
			Capacity: fmt.Sprintf("%d slots (`verify_slots`), no queue", front.DefaultVerifySlots), Workers: "the slots",
			Policy: "`shed` 503 + `Retry-After` at once", Deadline: deadline, Metric: q("verify"), Lever: limit("verify"),
			FirstLook: "the tenancy breaker and store latency (`mansio_breaker_state{dep=\"tenancy\"}`), then `credential_place`"},
		{Queue: "credential_place", Protects: "the placement authority (a credential request for a database not active here: claim or create)",
			Capacity: fmt.Sprintf("%d flights (`lifecycle_concurrency`), one per database; creates one at a time", c.LifecycleConcurrency),
			Workers:  "one goroutine per flight", Policy: "a request for another database is `shed` 503 at once; requests for a database in flight wait for it",
			Deadline: fmt.Sprintf("the flight: `lifecycle_timeout` %v; each waiter: its own request deadline", c.LifecycleTimeout),
			Metric:   q("credential_place"), Lever: limit("credential_place"),
			FirstLook: "placement store latency and its breaker; a first-use storm shows as refusals here with `verify` calm"},
		{Queue: "admission", Protects: "the actors and the CPU: statements in flight per database, tenant and VM",
			Capacity: fmt.Sprintf("queued: %d per database (`admission.db_queue`), %d per tenant (`tenant_queue`), 4 × in flight for the VM (`vm_queue`)",
				adm.DBQueue, adm.TenantQueue),
			Workers: fmt.Sprintf("in flight: %d per database (`db_in_flight`), %d per tenant (`tenant_in_flight`), 4 × GOMAXPROCS × db_in_flight for the VM (`vm_in_flight`)",
				adm.DBInFlight, adm.TenantInFlight),
			Policy:   "queues in arrival order; `shed` 503 + `Retry-After` when a queue is full or the request would finish after its deadline; `expired` 504 if it runs out while queued",
			Deadline: deadline, Metric: q("admission") + ", `mansio_admission`", Lever: capacity.NoLever + "; `mansioctl agent drain` sheds everything",
			FirstLook: "`reason`: `would_expire` means statements are slow (check `mansio_request_latency_seconds`), `full` means a burst beyond the queue"},
		{Queue: "mailbox", Protects: "one database's writer (its SQLite connection)",
			Capacity: fmt.Sprintf("%d per database (`actor.mailbox_size`)", act.MailboxSize),
			Workers:  fmt.Sprintf("one goroutine per database; a batch commits up to %d writes together (`actor.max_batch`)", act.MaxBatch),
			Policy:   "`shed` 503 at once; a request found past its deadline at pickup is `expired` without running",
			Deadline: fmt.Sprintf("the request's deadline; a statement at most `statement_timeout` %v", c.StatementTimeout),
			Metric:   q("mailbox") + ", `mansio_actor_queue_depth`", Lever: capacity.NoLever,
			FirstLook: "it sits behind admission's per-database cap, so refusals here mean admission's `db_in_flight` + `db_queue` exceed the mailbox: fix the config"},
		{Queue: "read", Protects: "one database's read connections",
			Capacity: fmt.Sprintf("%d per database (`actor.mailbox_size`), only with a read pool", act.MailboxSize),
			Workers:  fmt.Sprintf("`actor.read_conns` readers per database (default %d: reads share the writer)", act.ReadConns),
			Policy:   "`shed` 503 at once, so reads never stall the writes behind them", Deadline: "as the mailbox", Metric: q("read"), Lever: capacity.NoLever,
			FirstLook: "slow reads holding every reader: `mansio_request_latency_seconds`, then `statement_timeout`"},
		{Queue: "fetch", Protects: "S3 (cold database fetches) and the local disk and network",
			Capacity: fmt.Sprintf("%d activations queued (package default, no config key)", vactor.DefaultMaxFetchQueue),
			Workers:  fmt.Sprintf("%d fetches or %d GiB in flight", vactor.DefaultMaxFetches, vactor.DefaultMaxFetchBytes>>30),
			Policy:   "`unavailable` 503 + `Retry-After` at once; a database whose last activation failed fails fast for a jittered backoff (`reason=\"failure_backoff\"`)",
			Deadline: "the activation: 30 s plus size / 32 MiB/s", Metric: q("fetch") + ", `mansio_vactor{stat=\"fetch_*\"}`",
			Lever:     "`mansioctl agent fetch-limit N` (`POST /admin/fetch:limit?n=N`); `mansioctl agent breaker s3 trip` stops fetches",
			FirstLook: "`mansio_s3_latency_seconds` and the S3 breaker; lower the fetch limit if the disk or network is the bottleneck"},
		{Queue: "lifecycle", Protects: "S3 (a fence writes back) and the placement authority, for controld's claim, fence and forget",
			Capacity: fmt.Sprintf("%d slots (`lifecycle_concurrency`), no queue", c.LifecycleConcurrency), Workers: "the slots",
			Policy: "`unavailable` 503 + `Retry-After` at once", Deadline: fmt.Sprintf("`lifecycle_timeout` %v", c.LifecycleTimeout),
			Metric: q("lifecycle"), Lever: limit("lifecycle"),
			FirstLook: "what controld is doing (a restore moves many databases); slow fences are slow write-backs: `mansio_s3_latency_seconds`"},
		{Queue: "tenancy_admin", Protects: "the tenancy store (the tenant admin API on the query listener)",
			Capacity: fmt.Sprintf("%d slots, no queue (no config key)", tenancyhttp.DefaultConcurrency), Workers: "the slots",
			Policy: "503 `shed` + `Retry-After` at once", Deadline: "10 s per call; the body within `body_read_timeout`", Metric: q("tenancy_admin"),
			Lever: limit("tenancy_admin"), FirstLook: "the tenancy store's latency and breaker"},
		{Queue: "tenancy_auth", Protects: "the tenancy store and the password-hash CPU (sign-in and sign-up)",
			Capacity: fmt.Sprintf("%d slots, no queue; before them 1 sign-in per second per source, burst 10", tenancyhttp.DefaultConcurrency), Workers: "the slots",
			Policy: "503 at once (429 + `Retry-After` from the per-source limit)", Deadline: "10 s per call", Metric: q("tenancy_auth"),
			Lever: limit("tenancy_auth"), FirstLook: "a sign-in flood (429s) or slow password hashing; lower the limit to protect query CPU"},
		{Queue: "admin", Protects: "nothing remote: the operator's own controls on the loopback admin listener",
			Capacity: fmt.Sprintf("%d slots, no queue (fixed)", adminConcurrency), Workers: "the slots", Policy: "`unavailable` 503 at once",
			Deadline: fmt.Sprintf("`lifecycle_timeout` %v", c.LifecycleTimeout), Metric: q("admin"), Lever: capacity.NoLever,
			FirstLook: "a passivate or backup holding slots; they end at `lifecycle_timeout`"},
		{Queue: "write_back", Protects: "S3 (publishing warm databases)",
			Capacity: "not a queue: every dirty database is a candidate each tick", Workers: fmt.Sprintf("%d write-backs at once (package default)", vactor.DefaultWorkers),
			Policy:    "writes keep being acknowledged while write-back fails; the unpublished window grows (see the README's durability section)",
			Deadline:  fmt.Sprintf("`maintenance.write_back_timeout` %v per write-back", c.Maintenance.WriteBackTimeout),
			Metric:    "`mansio_vactor_dirty`, `mansio_vactor_oldest_dirty_age_seconds`, `mansio_vactor_events_total{event=\"writeback_failure\"}`",
			Lever:     "`mansioctl agent breaker s3 trip|reset`; `mansioctl agent backup KEY` forces one",
			FirstLook: "the S3 breaker and `mansio_s3_requests_total{class=\"5xx\"}`: the dirty age is the data a host loss would lose"},
		{Queue: "events", Protects: "the event log's disk writer and its volume; the request path never waits on it",
			Capacity: fmt.Sprintf("%d records (`event_log.buffer`); on disk %d GiB (`event_log.max_total_bytes`) and %d days (`event_log.max_age`)",
				eventlog.DefaultBuffer, eventlog.DefaultMaxTotalBytes>>30, int(eventlog.DefaultMaxAge.Hours()/24)),
			Workers:   "one writer goroutine",
			Policy:    "queue full: the newest record is dropped and counted, and the next write records how many; disk over its limits: the oldest files are deleted when a new one opens",
			Deadline:  "none (telemetry); at shutdown the writer gets 2 s and counts what it could not write as lost",
			Metric:    "`mansio_eventlog_queue_depth`, `mansio_eventlog_records_total{result=\"dropped|lost\"}`, `mansio_eventlog_bytes`, `mansio_eventlog_retired_total`",
			Lever:     capacity.NoLever,
			FirstLook: "a slow or full disk under `event_log.dir`; `mansio_eventlog_retired_total` rising means files are deleted at the writer's size or age limit, before a collector shipped them"},
	}
}

// The agentd section of docs/capacity.md is what the config defaults generate (run with -update after a change).
func TestCapacityTable(t *testing.T) {
	sec := capacity.Section("agentd", "the operator of the agent VM", capacityRows())
	if err := capacity.Check("../../docs/capacity.md", "agentd", sec, *updateCapacity); err != nil {
		t.Fatal(err)
	}
}

// Every mansio_* series an alert rule names is one agentd exports (a typo would make the alert silently
// dead): the families of a running static agent with an event log, plus durable mode's S3 and host families.
func TestAlertRulesUseRealSeries(t *testing.T) {
	rules, err := os.ReadFile("../../deploy/prometheus/backpressure.rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixture(t, []string{"db-1"}, `,"health_listen":"127.0.0.1:0","event_log":{"dir":"events"}`)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	waitAddr(t, addrFile, done)
	life := lifecycleAddr(t, addrFile)
	waitReady(t, life)
	res, err := http.Get("http://" + life + metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	static, _ := io.ReadAll(res.Body)
	res.Body.Close()
	cancel()
	<-done

	reg := metrics.NewRegistry()
	s3Metrics(reg, func() map[string]objstore.OpStats { return nil })
	metrics.Breakers(reg, nil)
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: durabletest.NewStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	vactorMetrics(reg, h, (&tickLog{}).lastTick)
	exported := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^# TYPE (\S+) `).FindAllStringSubmatch(string(static)+scrape(t, reg), -1) {
		exported[m[1]] = true
	}
	named := regexp.MustCompile(`mansio_[a-z0-9_]+`).FindAllString(string(rules), -1)
	if len(named) < 8 {
		t.Fatalf("only %d series names found in the rules", len(named))
	}
	for _, name := range named {
		if !exported[name] && !exported[strings.TrimSuffix(name, "_bucket")] {
			t.Errorf("the alert rules name %s, which agentd does not export", name)
		}
	}
}
