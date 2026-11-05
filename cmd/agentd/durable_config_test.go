package main

import (
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
	c, err := parseConfig(strings.NewReader(base+`"s3":{"endpoint":"e:1","bucket":"b"},"cache_bytes":1000}`), "/")
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
