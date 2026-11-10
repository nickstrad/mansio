package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/control/agenthttp"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/tlsx"
)

func TestConfig(t *testing.T) {
	agent := `{"vm":"vm-a","endpoint":"127.0.0.1:7070","lifecycle":"http://127.0.0.1:7071"}`
	base := `"secrets_file":"s.json","ticket_keys":{"dir":"keys"},"s3":{"endpoint":"e:1","bucket":"b"}`
	for _, tc := range []struct{ name, body, problem string }{
		{"minimal", `{` + base + `,"agents":[` + agent + `]}`, ""},
		{"no agents", `{` + base + `}`, "at least one"},
		{"duplicate agent", `{` + base + `,"agents":[` + agent + `,` + agent + `]}`, "twice"},
		{"bad vm", `{` + base + `,"agents":[{"vm":"VM","endpoint":"x","lifecycle":"http://x"}]}`, "not a valid vm id"},
		{"lifecycle not a URL", `{` + base + `,"agents":[{"vm":"a","endpoint":"x","lifecycle":"x:1"}]}`, "http(s) URL"},
		{"no bucket", `{"secrets_file":"s","s3":{"endpoint":"e"},"agents":[` + agent + `]}`, "endpoint and bucket"},
		{"no ticket keys dir", `{"secrets_file":"s","s3":{"endpoint":"e:1","bucket":"b"},"agents":[` + agent + `]}`, "ticket_keys.dir is required"},
		{"rotate = activate (off point)", `{` + base + `,"agents":[` + agent + `],"ticket_keys":{"dir":"k","rotate":"5m","activate":"5m"}}`, "must exceed"},
		{"zero ticket ttl", `{` + base + `,"agents":[` + agent + `],"locator":{"ticket_ttl":"0s"}}`, "locator.ticket_ttl must be > 0"},
		{"no resolve slots", `{` + base + `,"agents":[` + agent + `],"locator":{"concurrency":0}}`, "locator.concurrency"},
		{"no load slots", `{` + base + `,"agents":[` + agent + `],"locator":{"load_concurrency":0}}`, "locator.load_concurrency"},
		{"callers fill the pool (off point: 10 + 2 + 20 = 32)", `{` + base + `,"agents":[` + agent + `],"api_concurrency":20}`, "must be below pg.max_conns 32"},
		{"callers one below the pool (on point: 10 + 2 + 19 = 31)", `{` + base + `,"agents":[` + agent + `],"api_concurrency":19}`, ""},
		{"a small pool needs smaller limits", `{` + base + `,"agents":[` + agent + `],"pg":{"max_conns":20}}`, "must be below pg.max_conns 20"},
		{"heartbeat timeout = interval (off point)", `{` + base + `,"agents":[` + agent + `],"heartbeat_interval":"1s","heartbeat_timeout":"1s"}`, "shorter than heartbeat_interval"},
		{"per key below agent timeout", `{` + base + `,"agents":[` + agent + `],"per_key_timeout":"1s","agent_timeout":"2s"}`, "at least agent_timeout"},
		{"CA file without secure", `{` + base + `,"agents":[` + agent + `],"s3":{"endpoint":"e:1","bucket":"b","ca_file":"ca.pem"}}`, "ca_file needs secure"},
		{"unknown field", `{` + base + `,"agents":[` + agent + `],"x":1}`, "unknown field"},
		{"tls without key", `{` + base + `,"agents":[` + agent + `],"tls":{"cert_file":"c.pem"}}`, "cert_file and key_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseConfig(strings.NewReader(tc.body), "/etc")
			if tc.problem == "" {
				if err != nil || c.SecretsFile != "/etc/s.json" || c.TicketKeys.Dir != "/etc/keys" || c.Policy.MaxHeartbeatAge.Duration != 6*time.Second {
					t.Fatalf("%+v %v", c, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("err %v, want %q", err, tc.problem)
			}
		})
	}
}

// OS-11: plain-HTTP S3 needs -dev; HTTPS does not. A listener without tls needs -dev too.
func TestTLSRefusal(t *testing.T) {
	for _, tc := range []struct {
		secure, tls, dev, ok bool
	}{{false, true, false, false}, {false, true, true, true}, {true, true, false, true}, {true, true, true, true},
		{true, false, false, false}, {true, false, true, true}, {false, false, true, true}} {
		c := &Config{Dev: tc.dev}
		c.S3.Secure = tc.secure
		if tc.tls {
			c.TLS = &tlsx.Files{CertFile: "c", KeyFile: "k"}
		}
		if err := c.checkDev(); (err == nil) != tc.ok {
			t.Errorf("secure %v tls %v dev %v: %v", tc.secure, tc.tls, tc.dev, err)
		}
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.json")
	os.WriteFile(cfg, []byte(`{"secrets_file":"s","ticket_keys":{"dir":"k"},"s3":{"endpoint":"e:1","bucket":"b"},"agents":[{"vm":"a","endpoint":"x","lifecycle":"http://x"}]}`), 0o644)
	var errw strings.Builder
	if code := realMain(t.Context(), []string{"-config", cfg}, &strings.Builder{}, &errw); code != exitError || !strings.Contains(errw.String(), "pass -dev") {
		t.Fatalf("plain-HTTP S3 without -dev: exit %d %q", code, errw.String())
	}
}

func TestSecrets(t *testing.T) {
	dir := t.TempDir()
	good := `{"pg_dsn":"postgres://u:SUPERSECRET@h/db","control_token":"0123456789abcdef","admin_token":"0123456789abcdef","s3_access_key":"gc","s3_secret_key":"SUPERSECRET"}`
	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
		problem    string
	}{
		{"ok", good, 0o600, ""},
		{"world readable", good, 0o644, "chmod 600"},
		{"short admin token (15)", strings.Replace(good, `"admin_token":"0123456789abcdef"`, `"admin_token":"0123456789abcde"`, 1), 0o600, "at least 16"},
		{"no dsn", strings.Replace(good, `"pg_dsn":"postgres://u:SUPERSECRET@h/db",`, "", 1), 0o600, "required"},
	} {
		p := filepath.Join(dir, tc.name)
		os.WriteFile(p, []byte(tc.body), tc.mode)
		os.Chmod(p, tc.mode)
		_, err := loadSecrets(p)
		if tc.problem == "" {
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.problem) || strings.Contains(err.Error(), "SUPERSECRET") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// Every error class the controller can return gets its answer; dependency outages are never internal.
func TestClassify(t *testing.T) {
	w := func(err error) error { return errs.Wrap("control", err, "cannot do it") }
	for _, tc := range []struct {
		err    error
		code   string
		status int
	}{
		{w(control.ErrState), "conflict", 409},
		{w(control.ErrExists), "conflict", 409},
		{w(control.ErrNotDead), "conflict", 409},
		{w(owner.ErrMoving), "conflict", 409},
		{w(owner.ErrNotFound), "not_found", 404},
		{w(pg.ErrUnknownVM), "not_found", 404},
		{w(owner.ErrInvalid), "bad_request", 400},
		{w(control.ErrNoCapacity), "no_capacity", 503},
		{w(pg.ErrUnavailable), "unavailable", 503},
		{w(agenthttp.ErrUnavailable), "unavailable", 503},
		{w(fmt.Errorf("%w: x", control.ErrNoAgent)), "unavailable", 503},
		{w(errBadRequest("x")), "bad_request", 400},
		{errors.New("bug"), "", 0},
	} {
		got := classify(tc.err)
		if tc.code == "" {
			if got != nil {
				t.Errorf("%v: %+v, want internal", tc.err, got)
			}
			continue
		}
		if got == nil || got.code != tc.code || got.status != tc.status {
			t.Errorf("%v: %+v, want %s %d", tc.err, got, tc.code, tc.status)
		}
	}
}

func TestCommandUsage(t *testing.T) {
	var out, errw strings.Builder
	if code := realMain(t.Context(), []string{"move", "-secrets", "s"}, &out, &errw); code != exitUsage || !strings.Contains(errw.String(), "-db KEY") {
		t.Fatalf("move without -db: %d %q", code, errw.String())
	}
	if code := realMain(t.Context(), []string{}, &out, &errw); code != exitUsage {
		t.Fatalf("no config: %d", code)
	}
}

func TestShippedConfigsParse(t *testing.T) {
	paths, _ := filepath.Glob("../../configs/controld/*.json")
	if len(paths) == 0 {
		t.Fatal("no shipped configs")
	}
	for _, p := range paths {
		if _, err := loadConfig(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
