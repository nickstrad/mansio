package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
)

// lifecycleAddr is the addr-file's second line (the lifecycle listener).
func lifecycleAddr(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	f := strings.Fields(string(b))
	if len(f) < 2 {
		t.Fatalf("addr-file without a lifecycle address: %q", b)
	}
	return f[1]
}

func getStatus(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// waitReady polls /readyz until it answers 200.
func waitReady(t *testing.T, addr string) {
	t.Helper()
	for range 500 {
		if code, _ := getStatus(t, "http://"+addr+front.ReadyPath); code == http.StatusOK {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, body := getStatus(t, "http://"+addr+front.ReadyPath)
	t.Fatalf("not ready: %d %v", code, body)
}

func queryDB(t *testing.T, addr, db string) (*http.Response, front.Response) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"db": db, "kind": "read", "sql": fronttest.GetSQL, "args": []any{2}})
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.QueryPath, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+iss.Mint(t, iss.Claims(db, "vm-1", time.Now(), time.Minute)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out front.Response
	json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// RI-07 at the agent: a database that will not open does not stop the others from being served, its requests
// are refused unavailable (503 + Retry-After, never internal), and the agent turns ready once every database
// was tried; /livez and /readyz live on the lifecycle listener only.
func TestServesOpenDatabasesWhileOthersFail(t *testing.T) {
	cfg := fixture(t, []string{"db-1"}, `,"health_listen":"127.0.0.1:0","ready_interval":"50ms","ready_timeout":"30ms"`)
	os.WriteFile(filepath.Join(filepath.Dir(cfg), "dbs", "db-bad.db"), []byte(strings.Repeat("not a database ", 512)), 0o644)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	life := lifecycleAddr(t, addrFile)
	waitReady(t, life)

	if _, out := queryDB(t, addr, "db-1"); out.Code != front.OK || out.Count != 1 {
		t.Fatalf("db-1: %+v", out)
	}
	res, out := queryDB(t, addr, "db-bad")
	if out.Code != front.Unavailable || res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Fatalf("db-bad: %d %+v", res.StatusCode, out)
	}
	if code, body := getStatus(t, "http://"+life+front.LivePath); code != http.StatusOK || body["status"] != "live" {
		t.Fatalf("livez %d %v", code, body)
	}
	if code, _ := getStatus(t, "http://"+addr+front.ReadyPath); code != http.StatusNotFound {
		t.Fatalf("readyz on the query listener: %d, want 404 (it lives on the lifecycle listener)", code)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}
	logb, _ := os.ReadFile(filepath.Join(filepath.Dir(cfg), "agentd.log"))
	for _, ev := range []string{`"event":"AGENT.STARTUP_ROUND"`, `"event":"AGENT.READY"`, "db-bad"} {
		if !strings.Contains(string(logb), ev) {
			t.Errorf("log lacks %s:\n%s", ev, logb)
		}
	}
}

// Without a lifecycle listener, /livez and /readyz share the query listener.
func TestLifecycleOnQueryListenerWhenAlone(t *testing.T) {
	cfg := fixture(t, []string{"db-1"}, `,"ready_interval":"50ms","ready_timeout":"30ms"`)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	var stderr syncBuf
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	waitReady(t, addr)
	cancel()
	<-done
}

func TestBreakerCheck(t *testing.T) {
	b, err := breaker.New(breaker.Config{MinCalls: 1, CoolDown: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c := breakerCheck("s3", b)
	if c.Name != "s3_breaker" || c.Run(context.Background()) != nil {
		t.Fatal("closed breaker fails readiness")
	}
	p, _, _ := b.Allow(time.Now())
	b.Done(p, time.Now(), breaker.Failure)
	if err := c.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "open") {
		t.Fatalf("open breaker: %v", err)
	}
}

// Lifecycle config bounds: the drain is part of the shutdown (drain_timeout <= shutdown_budget), and a probe
// ends before the next starts (ready_timeout < ready_interval; Release It T4).
func TestLifecycleConfigBounds(t *testing.T) {
	for _, tc := range []struct {
		fields string
		ok     bool
	}{
		{`"drain_timeout":"1s","shutdown_budget":"1s"`, true},
		{`"drain_timeout":"1001ms","shutdown_budget":"1s"`, false},
		{`"drain_timeout":"10s","shutdown_budget":"60s"`, true},
		{`"ready_timeout":"999ms","ready_interval":"1s"`, true},
		{`"ready_timeout":"1s","ready_interval":"1s"`, false},
		{`"startup_workers":0`, false},
	} {
		_, err := parseConfig(strings.NewReader(`{"vm":"v","db_dir":"d","keys_file":"k",`+tc.fields+`}`), "/")
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v", tc.fields, err)
		}
	}
}
