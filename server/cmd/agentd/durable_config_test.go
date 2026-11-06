package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Durable-mode config: what it requires, what it refuses, and the cache split (cache_bytes → generation cache
// and warm budget).
func TestDurableConfig(t *testing.T) {
	base := `{"vm":"v","keys_file":"k","secrets_file":"s","cache_dir":"c","owner_dir":"o","placement_dir":"p",`
	for _, tc := range []struct {
		name, fields, problem string
	}{
		{"minimal", `"s3":{"endpoint":"e:1","bucket":"b"}`, ""},
		{"no bucket", `"s3":{"endpoint":"e:1"}`, "endpoint and bucket"},
		{"prefix without slash", `"s3":{"endpoint":"e:1","bucket":"b","prefix":"x"}`, "must end in /"},
		{"bad breaker", `"s3":{"endpoint":"e:1","bucket":"b","breaker":{"failure_rate":2}}`, "s3.breaker"},
		{"static fields", `"s3":{"endpoint":"e:1","bucket":"b"},"db_dir":"d"`, "static mode only"},
		{"gen cache = budget (off point)", `"s3":{"endpoint":"e:1","bucket":"b"},"cache_bytes":100,"gen_cache_bytes":100`, "below cache_bytes"},
		{"no lifecycle slots", `"s3":{"endpoint":"e:1","bucket":"b"},"lifecycle_concurrency":0`, "lifecycle_concurrency"},
		{"two authorities", `"s3":{"endpoint":"e:1","bucket":"b"},"placement_pg":{}`, "exactly one placement authority"},
		{"secure with a CA file", `"s3":{"endpoint":"e:1","bucket":"b","secure":true,"ca_file":"ca.pem"}`, ""},
		{"CA file without secure", `"s3":{"endpoint":"e:1","bucket":"b","ca_file":"ca.pem"}`, "ca_file needs secure"},
		{"admin on loopback v4", `"s3":{"endpoint":"e:1","bucket":"b"},"admin_listen":"127.0.0.1:7072"`, ""},
		{"admin on loopback v6", `"s3":{"endpoint":"e:1","bucket":"b"},"admin_listen":"[::1]:0"`, ""},
		{"admin on localhost", `"s3":{"endpoint":"e:1","bucket":"b"},"admin_listen":"localhost:7072"`, ""},
		{"admin on every interface", `"s3":{"endpoint":"e:1","bucket":"b"},"admin_listen":":7072"`, "loopback"},
		{"admin on a routable address", `"s3":{"endpoint":"e:1","bucket":"b"},"admin_listen":"10.0.0.5:7072"`, "loopback"},
		{"admin without a port", `"s3":{"endpoint":"e:1","bucket":"b"},"admin_listen":"127.0.0.1"`, "loopback"},
		{"schema registry", `"s3":{"endpoint":"e:1","bucket":"b"},"schemas_url":"https://controld:7443"`, ""},
		{"schema registry with the control CA", `"s3":{"endpoint":"e:1","bucket":"b"},"schemas_url":"https://c","control_ca_file":"ca.pem"`, ""},
		{"schema registry not a url", `"s3":{"endpoint":"e:1","bucket":"b"},"schemas_url":"controld:7443"`, "schemas_url"},
		{"schema cache 0 (off point)", `"s3":{"endpoint":"e:1","bucket":"b"},"schemas_url":"https://c","schema_cache":0`, "schema_cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseConfig(strings.NewReader(base+tc.fields+`}`), "/")
			if tc.problem == "" {
				if err != nil || !c.Durable() {
					t.Fatalf("%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("err %v, want %q", err, tc.problem)
			}
		})
	}
	static := `{"vm":"v","keys_file":"k","db_dir":"d","schemas_url":"https://c"}`
	if _, err := parseConfig(strings.NewReader(static), "/"); err == nil || !strings.Contains(err.Error(), "schemas_url needs durable mode") {
		t.Fatalf("schemas_url in static mode: %v", err)
	}
	plain, err := parseConfig(strings.NewReader(base+`"s3":{"endpoint":"e:1","bucket":"b","secure":true},"schemas_url":"http://c"}`), "/")
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.checkDev(); err == nil || !strings.Contains(err.Error(), "schemas_url is plain HTTP") {
		t.Fatalf("plain-HTTP schemas_url without -dev: %v", err)
	}
	c, err := parseConfig(strings.NewReader(base+`"s3":{"endpoint":"e:1","bucket":"b","secure":true,"ca_file":"ca.pem"}}`), "/etc/agentd")
	if err != nil || c.S3.CAFile != "/etc/agentd/ca.pem" {
		t.Fatalf("ca_file not resolved against the config's directory: %q %v", c.S3.CAFile, err)
	}
	c, err = parseConfig(strings.NewReader(base+`"s3":{"endpoint":"e:1","bucket":"b"},"cache_bytes":1000}`), "/")
	if err != nil || c.GenCacheBytes != 250 || c.Maintenance.MaxBytes != 750 {
		t.Fatalf("cache split: gen %d warm %d, %v", c.GenCacheBytes, c.Maintenance.MaxBytes, err)
	}
	pgOnly := `{"vm":"v","keys_file":"k","secrets_file":"s","cache_dir":"c","owner_dir":"o","s3":{"endpoint":"e:1","bucket":"b"},"placement_pg":{}}`
	if c, err := parseConfig(strings.NewReader(pgOnly), "/"); err != nil || c.PlacementPG.MaxConns != 32 {
		t.Fatalf("placement_pg: pool %+v, %v (want startup_workers + lifecycle_concurrency = 32)", c.PlacementPG, err)
	}
	badBreaker := strings.Replace(pgOnly, `"placement_pg":{}`, `"placement_pg":{"breaker":{"failure_rate":2}}`, 1)
	if _, err := parseConfig(strings.NewReader(badBreaker), "/"); err == nil || !strings.Contains(err.Error(), "placement_pg.breaker") {
		t.Fatalf("bad placement breaker: %v", err)
	}
	noAuthority := strings.Replace(pgOnly, `,"placement_pg":{}`, "", 1)
	if _, err := parseConfig(strings.NewReader(noAuthority), "/"); err == nil || !strings.Contains(err.Error(), "exactly one placement authority") {
		t.Fatalf("no authority: %v", err)
	}
	if _, err := parseConfig(strings.NewReader(`{"vm":"v","keys_file":"k","s3":{"endpoint":"e:1","bucket":"b"}}`), "/"); err == nil ||
		!strings.Contains(err.Error(), "secrets_file is required") {
		t.Fatalf("missing dirs: %v", err)
	}
}

// OS-11 (object-storage guide S2): durable mode over plain-HTTP S3 is refused without -dev, at startup with
// exit 1 and a message saying what to do; -dev, s3.secure and static mode are accepted.
func TestPlainHTTPS3NeedsDev(t *testing.T) {
	for _, tc := range []struct {
		name, s3 string
		dev      bool
		problem  string
	}{
		{"plain, no -dev", `{"endpoint":"e:1","bucket":"b"}`, false, "s3.secure is false"},
		{"plain, -dev", `{"endpoint":"e:1","bucket":"b"}`, true, ""},
		{"secure, no -dev", `{"endpoint":"e:1","bucket":"b","secure":true}`, false, ""},
		{"static mode, no -dev", ``, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"vm":"v","keys_file":"k","db_dir":"d"}`
			if tc.s3 != "" {
				body = `{"vm":"v","keys_file":"k","secrets_file":"s","cache_dir":"c","owner_dir":"o","placement_dir":"p","s3":` + tc.s3 + `}`
			}
			c, err := parseConfig(strings.NewReader(body), "/")
			if err != nil {
				t.Fatal(err)
			}
			c.Dev = tc.dev
			err = c.checkDev()
			if tc.problem == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.problem) || !strings.Contains(err.Error(), "-dev") {
				t.Fatalf("err %v, want %q and a hint", err, tc.problem)
			}
		})
	}

	// End to end: agentd itself refuses to start (exit 1) without -dev, before touching anything.
	r := newDurableRig(t, "")
	var stderr bytes.Buffer
	if code := realMain(context.Background(), []string{"-config", r.cfg}, &stderr); code != exitError ||
		!strings.Contains(stderr.String(), "s3.secure is false") {
		t.Fatalf("exit %d, stderr %q; want 1 and the TLS refusal", code, stderr.String())
	}
	stderr.Reset()
	if code := realMain(context.Background(), []string{"-dev", "-check", "-config", r.cfg}, &stderr); code != exitOK {
		t.Fatalf("-dev -check: exit %d, %s", code, stderr.String())
	}
}

// The secrets file must be private, complete and strict; its errors never echo a secret.
func TestLoadSecrets(t *testing.T) {
	dir := t.TempDir()
	write := func(body string, mode os.FileMode) string {
		p := filepath.Join(dir, "s.json")
		os.Remove(p)
		os.WriteFile(p, []byte(body), mode)
		os.Chmod(p, mode)
		return p
	}
	good := `{"s3_access_key":"AK","s3_secret_key":"SUPERSECRET","control_token":"0123456789abcdef"}`
	if s, err := loadSecrets(write(good, 0o600)); err != nil || s.S3SecretKey != "SUPERSECRET" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
		problem    string
	}{
		{"group readable", good, 0o640, "chmod 600"},
		{"unknown field", `{"s3_access_key":"AK","s3_secret_key":"SUPERSECRET","control_token":"0123456789abcdef","x":1}`, 0o600, "not valid"},
		{"no secret key", `{"s3_access_key":"AK","control_token":"0123456789abcdef"}`, 0o600, "required"},
		{"short token (off point: 15)", `{"s3_access_key":"AK","s3_secret_key":"SUPERSECRET","control_token":"0123456789abcde"}`, 0o600, "at least 16"},
	} {
		_, err := loadSecrets(write(tc.body, tc.mode))
		if err == nil || !strings.Contains(err.Error(), tc.problem) || strings.Contains(err.Error(), "SUPERSECRET") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if _, err := loadSecrets(filepath.Join(dir, "none")); err == nil {
		t.Fatal("missing file accepted")
	}
}
