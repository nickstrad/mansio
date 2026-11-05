package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigDefaultsAndPaths(t *testing.T) {
	c, err := parseConfig(strings.NewReader(`{"keys_file":"k.json","tls":{"cert_file":"e.crt","key_file":"/abs/e.key"},
		"agents":{"vm-a":"https://10.0.0.5:7070"},"agent_tls":{"ca_file":"ca.pem"}}`), "/etc/edge")
	if err != nil {
		t.Fatal(err)
	}
	if c.KeysFile != "/etc/edge/k.json" || c.TLS.CertFile != "/etc/edge/e.crt" || c.TLS.KeyFile != "/abs/e.key" ||
		c.AgentTLS.CAFile != "/etc/edge/ca.pem" {
		t.Fatalf("paths: %+v %+v %+v", c.KeysFile, c.TLS, c.AgentTLS)
	}
	if c.RateLimit.PerSecond != 20 || c.DefaultTimeout.Duration.Seconds() != 2 || c.MaxBody != 1<<20 {
		t.Fatalf("defaults: %+v", c)
	}
	if err := c.checkDev(); err != nil {
		t.Fatalf("TLS on both hops refused: %v", err)
	}
}

// Every problem is reported at once.
func TestParseConfigProblems(t *testing.T) {
	_, err := parseConfig(strings.NewReader(`{"listen":"","tls":{"cert_file":"x"},"agents":{"":"https://a:1","vm-b":"https://b",
		"vm-c":"https://c:1/path","vm-d":"tcp://d:1"},"rate_limit":{"burst":0.5},"default_timeout":"40s","drain_timeout":"1m",
		"agent_breaker":{"failure_rate":2},"log_level":"loud"}`), ".")
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"listen and health_listen", "cert_file and key_file", "keys_file is required", "vm id \"\"",
		"want scheme://host:port", "no path", "scheme must be https", "rate_limit", "default_timeout 40s is over max_timeout",
		"drain_timeout 1m0s is over shutdown_budget", "agent_breaker", "log_level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	if _, err := parseConfig(strings.NewReader(`{"surprise":1}`), "."); err == nil {
		t.Error("unknown field accepted")
	}
}

// Plain HTTP on either hop needs -dev; a CA file nothing would use is refused even with -dev.
func TestCheckDev(t *testing.T) {
	tlsOn := &TLSFiles{CertFile: "c", KeyFile: "k"}
	for _, tc := range []struct {
		name   string
		c      Config
		ok     bool
		reason string
	}{
		{"tls both hops", Config{TLS: tlsOn, Agents: map[string]string{"a": "https://a:1"}}, true, ""},
		{"no tls", Config{Agents: map[string]string{"a": "https://a:1"}}, false, "plain HTTP to clients"},
		{"no tls, dev", Config{Dev: true, Agents: map[string]string{"a": "https://a:1"}}, true, ""},
		{"one http agent", Config{TLS: tlsOn, Agents: map[string]string{"a": "https://a:1", "b": "http://b:1"}}, false, "1 agent URLs are plain http"},
		{"http agent, dev", Config{TLS: tlsOn, Dev: true, Agents: map[string]string{"b": "http://b:1"}}, true, ""},
		{"unused ca", Config{Dev: true, AgentTLS: AgentTLS{CAFile: "ca"}, Agents: map[string]string{"b": "http://b:1"}}, false, "unused"},
	} {
		err := tc.c.checkDev()
		if (err == nil) != tc.ok || (err != nil && !strings.Contains(err.Error(), tc.reason)) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestAgentTransportCA(t *testing.T) {
	dir := t.TempDir()
	c := defaults()
	c.AgentTLS.CAFile = filepath.Join(dir, "none.pem")
	if _, err := c.agentTransport(); err == nil {
		t.Error("missing CA file accepted")
	}
	os.WriteFile(c.AgentTLS.CAFile, []byte("not pem"), 0o644)
	if _, err := c.agentTransport(); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("bad CA file: %v", err)
	}
}

// The shipped configs parse, and local.json is the -dev one: tls.json passes checkDev without it.
func TestShippedConfigsParse(t *testing.T) {
	paths, _ := filepath.Glob("../../configs/edge/*.json")
	if len(paths) < 2 {
		t.Fatal("no shipped configs")
	}
	for _, p := range paths {
		c, err := loadConfig(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if dev := c.checkDev() != nil; dev != (filepath.Base(p) == "local.json") {
			t.Errorf("%s: needs -dev = %v", p, dev)
		}
	}
}
