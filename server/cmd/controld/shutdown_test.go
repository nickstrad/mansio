package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// At shutdown controld fails /readyz and keeps serving for drain_delay, then stops accepting; a call
// still in flight when shutdown_budget ends is cut (the listener's connections are closed), so the process
// ends inside its budget. Postgres is unreachable here: the API answers unavailable, which is all this needs.
func TestShutdownDrainDelayAndBudget(t *testing.T) {
	dir := t.TempDir()
	sec, _ := json.Marshal(Secrets{PGDSN: "postgres://x:y@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", ControlToken: controlToken,
		AdminToken: adminToken, S3AccessKey: "gc", S3SecretKey: "gc-secret-dev"})
	os.WriteFile(filepath.Join(dir, "secrets.json"), sec, 0o600)
	const delay, budget = 500 * time.Millisecond, 1500 * time.Millisecond
	cfg := `{"listen":"127.0.0.1:0","secrets_file":"secrets.json","log_file":"controld.log","ticket_keys":{"dir":"keys"},
		"s3":{"endpoint":"127.0.0.1:1","bucket":"b"},"agents":[{"vm":"vm-a","endpoint":"127.0.0.1:1","lifecycle":"http://127.0.0.1:1"}],
		"drain_delay":"500ms","shutdown_budget":"1500ms"}`
	path, addrFile := filepath.Join(dir, "controld.json"), filepath.Join(dir, "addr")
	os.WriteFile(path, []byte(cfg), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		done <- realMain(ctx, []string{"-dev", "-config", path, "-addr-file", addrFile}, io.Discard, &stderr)
	}()
	var addr string
	for deadline := time.Now().Add(10 * time.Second); addr == ""; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(addrFile); err == nil && len(b) > 0 {
			addr = strings.TrimSpace(string(b))
		}
		select {
		case c := <-done:
			t.Fatalf("controld exited %d: %s", c, &stderr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("controld did not serve: %s", &stderr)
		}
	}
	get := func(path string) (int, string) {
		res, err := (&http.Client{Transport: &http.Transport{}}).Get("http://" + addr + path) // a new connection each time
		if err != nil {
			return 0, err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	// A call that never finishes sending its body is still in flight when the budget ends.
	stuck, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()
	fmt.Fprintf(stuck, "POST /v1/databases HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100\r\n\r\n{", adminToken)

	t0 := time.Now()
	cancel()
	for {
		if code, body := get("/readyz"); code == 503 && strings.Contains(body, "stopping") {
			break
		}
		if time.Since(t0) > delay {
			t.Fatal("/readyz did not say stopping during drain_delay")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code, body := get("/livez"); code != 200 {
		t.Fatalf("a new connection during drain_delay (%v after shutdown began): %d %s", time.Since(t0), code, body)
	}
	if c := <-done; c != exitError || !strings.Contains(stderr.String(), "shutdown did not finish within") {
		t.Fatalf("exit %d, want the budget overrun reported: %s", c, &stderr)
	}
	if took := time.Since(t0); took < budget || took > budget+2*time.Second {
		t.Fatalf("shutdown took %v, want the %v budget (drain_delay included) and no more", took, budget)
	}
	if code, _ := get("/livez"); code != 0 {
		t.Fatalf("the listener still answers after shutdown: %d", code)
	}
	// The stuck call's connection was closed, not left open past the budget.
	stuck.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := stuck.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Fatalf("the in-flight connection is still open after the budget: %v", err)
	}
}
