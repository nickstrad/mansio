//go:build e2e

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/edge/edgetest"
)

// End to end with the real binaries and a private CA, no -dev: frontbench → (TLS, h2) edge → (TLS, h2) agentd.
// Every request answers ok through the edge; SIGTERM stops both with exit 0. Run with:
// go test -tags e2e ./cmd/edge
func TestFrontbenchThroughEdge(t *testing.T) {
	dir := t.TempDir()
	bin := func(pkg, name string) string {
		out := filepath.Join(dir, name)
		if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
		return out
	}
	agentd, edgeBin, fb := bin("../agentd", "agentd"), bin(".", "edge"), bin("../frontbench", "frontbench")
	must := func(name string, args ...string) string {
		b, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, b)
		}
		return string(b)
	}
	must(fb, "keygen", "-out", filepath.Join(dir, "keys"))
	must(fb, "seed", "-dir", filepath.Join(dir, "dbs"), "-dbs", "4")
	ca := edgetest.NewCA(t)
	caFile, agentCert, agentKey := ca.WriteFiles(t, dir, "agent", "127.0.0.1")
	_, edgeCert, edgeKey := ca.WriteFiles(t, dir, "edge", "127.0.0.1", "localhost")

	startProc := func(name, bin, cfg string) (*exec.Cmd, []string, *strings.Builder) {
		path := filepath.Join(dir, name+".json")
		os.WriteFile(path, []byte(cfg), 0o644)
		addrFile := filepath.Join(dir, name+".addr")
		cmd := exec.Command(bin, "-config", path, "-addr-file", addrFile)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() })
		for range 500 {
			if b, err := os.ReadFile(addrFile); err == nil && strings.Count(string(b), "\n") >= 2 {
				return cmd, strings.Fields(string(b)), &stderr
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s did not start:\n%s", name, stderr.String())
		return nil, nil, nil
	}
	agent, aAddr, aErr := startProc("agentd", agentd, fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0",
		"vm":"vm-local","db_dir":"dbs","keys_file":"keys/keys.json","log_file":"agentd.log","tls":{"cert_file":%q,"key_file":%q}}`,
		agentCert, agentKey))
	edge, eAddr, eErr := startProc("edge", edgeBin, fmt.Sprintf(`{"listen":"127.0.0.1:0","health_listen":"127.0.0.1:0",
		"keys_file":"keys/keys.json","log_file":"edge.log","tls":{"cert_file":%q,"key_file":%q},
		"agents":{"vm-local":"https://%s"},"agent_tls":{"ca_file":%q}}`, edgeCert, edgeKey, aAddr[0], caFile))

	out := must(fb, "run", "-mode", "http", "-proto", "h2", "-url", "https://"+eAddr[0], "-ca", caFile,
		"-key", filepath.Join(dir, "keys", "issuer.key"), "-dbs", "4", "-conc", "8", "-duration", "1s", "-warmup", "200ms", "-write", "0.2")
	if m := regexp.MustCompile(`codes map\[([^\]]*)\]`).FindStringSubmatch(out); m == nil || strings.Contains(m[1], " ") ||
		!strings.HasPrefix(m[1], "ok:") {
		t.Fatalf("frontbench through the edge: want only ok codes\n%s", out)
	}
	t.Log(out)
	for _, p := range []struct {
		name   string
		cmd    *exec.Cmd
		stderr *strings.Builder
	}{{"edge", edge, eErr}, {"agentd", agent, aErr}} {
		p.cmd.Process.Signal(syscall.SIGTERM)
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("%s exit: %v\n%s", p.name, err, p.stderr.String())
		}
	}
}
