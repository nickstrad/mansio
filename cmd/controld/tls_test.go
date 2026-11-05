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
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/edge/edgetest"
	"github.com/nickstrad/ts-db-go/internal/keyfetch"
)

// startTLSControld runs controld on a loopback listener with cfgExtra in its config and args before -config.
// Postgres is unreachable on purpose: GET /v1/keys and /livez answer before startup is done. It returns the
// exit code channel and the address once serving ("" if it exited first).
func startTLSControld(t *testing.T, dir, cfgExtra string, args ...string) (addr string, code <-chan int, stderr *bytes.Buffer) {
	t.Helper()
	sec, _ := json.Marshal(Secrets{PGDSN: "postgres://x:y@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", ControlToken: controlToken, AdminToken: adminToken,
		S3AccessKey: "gc", S3SecretKey: "gc-secret-dev"})
	os.WriteFile(filepath.Join(dir, "secrets.json"), sec, 0o600)
	cfg := `{"listen":"127.0.0.1:0","secrets_file":"secrets.json","log_file":"controld.log","ticket_keys":{"dir":"keys"},
		"s3":{"endpoint":"127.0.0.1:1","bucket":"b","secure":true},"agents":[{"vm":"vm-a","endpoint":"127.0.0.1:1","lifecycle":"http://127.0.0.1:1"}]` +
		cfgExtra + `}`
	path := filepath.Join(dir, "controld.json")
	os.WriteFile(path, []byte(cfg), 0o644)
	addrFile := filepath.Join(dir, "addr")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	stderr = &bytes.Buffer{}
	go func() {
		done <- realMain(ctx, append(args, "-config", path, "-addr-file", addrFile), io.Discard, stderr)
	}()
	t.Cleanup(func() { cancel(); <-done })
	exited := make(chan int, 1)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case c := <-done:
			exited <- c
			done <- c // for the cleanup
			return "", exited, stderr
		default:
		}
		if b, err := os.ReadFile(addrFile); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b)), done, stderr
		}
	}
	t.Fatalf("controld did not serve: %s", stderr)
	return
}

// The resolve, keys and admin API over HTTPS with a private CA; a plain listener only with -dev.
func TestListenerTLS(t *testing.T) {
	dir := t.TempDir()
	ca := edgetest.NewCA(t)
	caFile, cert, key := ca.WriteFiles(t, dir, "controld", "127.0.0.1")

	if _, code, stderr := startTLSControld(t, t.TempDir(), ""); <-code != exitError || !strings.Contains(stderr.String(), "no tls section") {
		t.Fatalf("plain listener without -dev: %s", stderr)
	}

	addr, _, stderr := startTLSControld(t, dir, `,"tls":{"cert_file":"`+cert+`","key_file":"`+key+`"}`)
	if addr == "" {
		t.Fatalf("controld with tls exited: %s", stderr)
	}
	// The agents' and the edge's key fetcher with control_ca_file.
	ring, f, err := keyfetch.Open(context.Background(), keyfetch.Config{URL: "https://" + addr + "/v1/keys", Every: time.Minute,
		Timeout: 3 * time.Second, CAFile: caFile, Log: slog.New(slog.DiscardHandler), Event: "TEST"})
	if err != nil || ring.Len() != 1 || f.Failing() {
		t.Fatalf("keys over TLS: %v keys, failing %v, %v", ring.Len(), f != nil && f.Failing(), err)
	}
	// Without the CA, and in plain HTTP, nothing is served.
	if _, err := http.Get("https://" + addr + "/livez"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("system roots accepted a private-CA certificate: %v", err)
	}
	if res, err := http.Get("http://" + addr + "/v1/keys"); err == nil && res.StatusCode == http.StatusOK {
		t.Fatal("plain HTTP served on the TLS listener")
	}
	// The CLI with -ca (the admin call itself answers unavailable: Postgres is unreachable here).
	var out, errOut bytes.Buffer
	if code := realMain(context.Background(), []string{"vms", "-addr", "https://" + addr, "-secrets", filepath.Join(dir, "secrets.json"),
		"-ca", caFile, "-timeout", "5s"}, &out, &errOut); code != exitError || !strings.Contains(out.String(), `"unavailable"`) {
		t.Fatalf("cli over TLS: exit %d, %s %s", code, out.String(), errOut.String())
	}
	if code := realMain(context.Background(), []string{"vms", "-addr", "https://" + addr, "-secrets", filepath.Join(dir, "secrets.json"),
		"-timeout", "5s"}, &out, &errOut); code != exitError || !strings.Contains(errOut.String(), "no answer") {
		t.Fatalf("cli without -ca: exit %d, %s", code, errOut.String())
	}
}
