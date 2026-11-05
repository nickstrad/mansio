package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
	"github.com/nickstrad/ts-db-go/internal/ticket"
	"github.com/nickstrad/ts-db-go/internal/ticket/tickettest"
)

var iss = tickettest.NewIssuer("k1")

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fixture writes a db dir with kv databases, a key file and a config; it returns the config path.
func fixture(t *testing.T, dbs []string, extra string) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "dbs"), 0o755)
	for _, db := range dbs {
		err := actor.Seed(filepath.Join(dir, "dbs", db+".db"), actor.Config{}, fronttest.KVSchema, func(exec func(string, ...any) error) error {
			for _, r := range fronttest.KVRows {
				if err := exec(fronttest.PutSQL, r...); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	keys, _ := json.Marshal(ticket.PublicKeyFile{Keys: []ticket.PublicKeyEntry{{KID: "k1", PublicKey: ticket.EncodeKey(iss.Pub)}}})
	os.WriteFile(filepath.Join(dir, "keys.json"), keys, 0o644)
	cfg := `{"listen":"127.0.0.1:0","vm":"vm-1","db_dir":"dbs","keys_file":"keys.json","log_file":"agentd.log"` + extra + `}`
	p := filepath.Join(dir, "agentd.json")
	os.WriteFile(p, []byte(cfg), 0o644)
	return p
}

var logID = regexp.MustCompile(`log ID ([0-9a-f]{8})`)

func TestExitCodes(t *testing.T) {
	good := fixture(t, []string{"db-1"}, "")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	tests := []struct {
		name   string
		ctx    context.Context
		args   []string
		code   int
		stderr string
	}{
		{"no flags", context.Background(), nil, exitUsage, "usage"},
		{"unknown flag", context.Background(), []string{"-nope"}, exitUsage, "flag provided but not defined"},
		{"stray arg", context.Background(), []string{"-config", good, "x"}, exitUsage, "usage"},
		{"missing config", context.Background(), []string{"-config", "/no/such.json"}, exitError, "cannot open config"},
		{"unknown field", context.Background(), []string{"-config", write(`{"vm":"v","nope":1}`)}, exitError, "unknown field"},
		{"every problem at once", context.Background(), []string{"-config", write(`{"listen":"","protocols":["h3"],"default_epoch":0,"log_level":"loud"}`)},
			exitError, "vm is required"},
		{"bad keys file", context.Background(), []string{"-config", fixture(t, []string{"db-1"}, `,"keys_file":"agentd.json"`)}, exitError, "invalid keys_file"},
		{"no databases", context.Background(), []string{"-config", fixture(t, nil, "")}, exitError, "no *.db files"},
		{"listed db missing", context.Background(), []string{"-config", fixture(t, []string{"db-1"}, `,"dbs":["db-2"]`)}, exitError, "missing"},
		{"check", context.Background(), []string{"-config", good, "-check"}, exitOK, "config ok: vm vm-1, 1 databases, 1 keys (k1)"},
		{"interrupted before serving", cancelled, []string{"-config", good}, exitInterrupted, "interrupted"},
		{"port in use", context.Background(), []string{"-config", fixture(t, []string{"db-1"}, `,"listen":"`+busy.Addr().String()+`"`)}, exitError, "cannot listen"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr syncBuf
			code := realMain(tc.ctx, tc.args, &stderr)
			out := stderr.String()
			if code != tc.code || !strings.Contains(out, tc.stderr) {
				t.Fatalf("exit %d (want %d), stderr:\n%s\nwant it to contain %q", code, tc.code, out, tc.stderr)
			}
			if strings.Count(out, "error: ") > 1 || strings.Contains(out, "goroutine ") {
				t.Fatalf("reported twice or leaked a stack:\n%s", out)
			}
		})
	}
}

// Start, serve a request over HTTP, check /healthz on its own listener, then stop on cancel: exit 0.
func TestServesAndStopsCleanly(t *testing.T) {
	cfg := fixture(t, []string{"db-1", "db-2"}, `,"health_listen":"127.0.0.1:0","epochs":{"db-2":7}`)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	waitReady(t, lifecycleAddr(t, addrFile))

	tok := func(db string, epoch uint64) string {
		c := iss.Claims(db, "vm-1", time.Now(), time.Minute)
		c.Epoch = epoch
		return iss.Mint(t, c)
	}
	query := func(db string, epoch uint64) front.Response {
		b, _ := json.Marshal(map[string]any{"db": db, "kind": "read", "sql": fronttest.GetSQL, "args": []any{2}})
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.QueryPath, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tok(db, epoch))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out front.Response
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if out := query("db-1", 1); out.Code != front.OK || out.Count != 1 || out.Rows[0][1] != "v2" {
		t.Fatalf("db-1: %+v", out)
	}
	if out := query("db-2", 1); out.Code != front.WrongOwner {
		t.Fatalf("db-2 at epoch 1 (owned at 7): %+v", out)
	}
	if out := query("db-2", 7); out.Code != front.OK {
		t.Fatalf("db-2 at epoch 7: %+v", out)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "agentd: stopped") {
		t.Fatalf("stderr:\n%s", stderr.String())
	}
	logb, _ := os.ReadFile(filepath.Join(filepath.Dir(cfg), "agentd.log"))
	if !strings.Contains(string(logb), `"msg":"serving"`) || !strings.Contains(string(logb), `"msg":"stopped"`) {
		t.Fatalf("log:\n%s", logb)
	}
}

// A request that outlives drain_timeout makes the stop a failure (exit 1, log ID), not a silent cut.
func TestDrainTimeoutIsReported(t *testing.T) {
	cfg := fixture(t, []string{"db-1"}, `,"drain_timeout":"50ms","statement_timeout":"2s","default_timeout":"5s"`)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	b, _ := json.Marshal(map[string]any{"db": "db-1", "kind": "read", "sql": fronttest.SlowSQL})
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.QueryPath, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+iss.Mint(t, iss.Claims("db-1", "vm-1", time.Now(), time.Minute)))
	go http.DefaultClient.Do(req)
	time.Sleep(100 * time.Millisecond) // the slow query is running
	cancel()
	code := <-done
	m := logID.FindStringSubmatch(stderr.String())
	if code != exitError || m == nil || !strings.Contains(stderr.String(), "drain did not finish") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}
	logb, _ := os.ReadFile(filepath.Join(filepath.Dir(cfg), "agentd.log"))
	if !strings.Contains(string(logb), m[1]) {
		t.Fatalf("log ID %s not in the log", m[1])
	}
}

func waitAddr(t *testing.T, path string, done chan int) string {
	t.Helper()
	for range 500 {
		select {
		case code := <-done:
			t.Fatalf("agentd exited %d before serving", code)
		default:
		}
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.Fields(string(b))[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agentd did not start")
	return ""
}

func TestMalformedErrorIsABug(t *testing.T) {
	var stderr, log bytes.Buffer
	rep := reporter{log: slog.New(slog.NewJSONHandler(&log, nil)), stderr: &stderr}
	if code := rep.report(errors.New("raw")); code != exitBug {
		t.Fatalf("exit %d", code)
	}
	m := logID.FindStringSubmatch(stderr.String())
	if m == nil || strings.Contains(stderr.String(), "raw") || !strings.Contains(log.String(), m[1]) {
		t.Fatalf("stderr %s log %s", stderr.String(), log.String())
	}
	stderr.Reset()
	if code := (reporter{stderr: &stderr}).bug(errors.New("raw"), "stack here"); code != exitBug || !strings.Contains(stderr.String(), "stack here") {
		t.Fatalf("bug before log: %s", stderr.String())
	}
}

func TestShippedConfigsParse(t *testing.T) {
	paths, _ := filepath.Glob("../../configs/agentd/*.json")
	if len(paths) == 0 {
		t.Fatal("no shipped configs")
	}
	for _, p := range paths {
		if _, err := loadConfig(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// Admission caps: VM defaults derive from the file's db_in_flight, and caps must nest db <= tenant <= VM.
func TestAdmissionCapsNest(t *testing.T) {
	base := `{"vm":"v","db_dir":"d","keys_file":"k","admission":`
	tests := []struct {
		name, admission, problem string
	}{
		{"defaults", `{}`, ""},
		{"vm default follows db_in_flight", `{"db_in_flight":100,"tenant_in_flight":200}`, ""},
		{"tenant = vm (on point)", `{"db_in_flight":1,"tenant_in_flight":4,"vm_in_flight":4}`, ""},
		{"tenant > vm (off point)", `{"db_in_flight":1,"tenant_in_flight":5,"vm_in_flight":4}`, "caps must nest"},
		{"db > tenant", `{"db_in_flight":9,"tenant_in_flight":4}`, "caps must nest"},
		{"negative vm queue", `{"vm_queue":-1}`, "caps must be >= 0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseConfig(strings.NewReader(base+tc.admission+`}`), "/")
			if tc.problem == "" {
				if err != nil {
					t.Fatal(err)
				}
				if a := c.Admission; a.VMInFlight < a.TenantInFlight || a.VMQueue <= 0 || a.VMInFlight%a.DBInFlight != 0 {
					t.Fatalf("admission %+v", a)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("err %v, want %q", err, tc.problem)
			}
		})
	}
}

// The health listener bounds every phase of a request (it has no body and a tiny answer).
func TestHealthServerHasTimeouts(t *testing.T) {
	c, err := parseConfig(strings.NewReader(`{"vm":"v","db_dir":"d","keys_file":"k"}`), "/")
	if err != nil {
		t.Fatal(err)
	}
	hs := healthServer(c, http.NewServeMux())
	if hs.ReadHeaderTimeout <= 0 || hs.ReadTimeout <= 0 || hs.WriteTimeout <= 0 || hs.IdleTimeout <= 0 || hs.MaxHeaderBytes <= 0 {
		t.Fatalf("health server without a timeout: %+v", hs)
	}
}
