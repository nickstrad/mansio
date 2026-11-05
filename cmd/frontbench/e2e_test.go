//go:build e2e

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// End to end with the real binaries: keygen, seed, start agentd, run frontbench over h1 and h2c, stop agentd
// with SIGTERM (exit 0 after a drain). Run with: go test -tags e2e ./cmd/frontbench
func TestAgentdAndFrontbench(t *testing.T) {
	dir := t.TempDir()
	bin := func(pkg, name string) string {
		out := filepath.Join(dir, name)
		if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
		return out
	}
	agentd, fb := bin("../agentd", "agentd"), bin(".", "frontbench")
	must := func(name string, args ...string) string {
		b, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, b)
		}
		return string(b)
	}
	must(fb, "keygen", "-out", filepath.Join(dir, "keys"))
	must(fb, "seed", "-dir", filepath.Join(dir, "dbs"), "-dbs", "4")
	cfg := filepath.Join(dir, "agentd.json")
	os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0","vm":"vm-local","db_dir":"dbs",
		"keys_file":"keys/keys.json","log_file":"agentd.log"}`), 0o644)
	addrFile := filepath.Join(dir, "addr")
	cmd := exec.Command(agentd, "-config", cfg, "-addr-file", addrFile)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var addr string
	for range 300 {
		if b, err := os.ReadFile(addrFile); err == nil && len(b) > 0 {
			addr = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("agentd did not start:\n%s", stderr.String())
	}
	for _, proto := range []string{"h1", "h2c"} {
		out := must(fb, "run", "-mode", "http", "-proto", proto, "-url", "http://"+addr, "-key", filepath.Join(dir, "keys", "issuer.key"),
			"-dbs", "4", "-conc", "8", "-duration", "300ms", "-warmup", "50ms", "-write", "0.2")
		if !strings.Contains(out, "http/"+proto) || !strings.Contains(out, "ok:") {
			t.Fatalf("%s run: %s", proto, out)
		}
	}
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	if cmd.ProcessState.ExitCode() != 0 || !strings.Contains(stderr.String(), "agentd: stopped") {
		t.Fatalf("agentd exit %d:\n%s", cmd.ProcessState.ExitCode(), stderr.String())
	}
}
