package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const appToken = "app-token-0123456789"

// tenancyFixture is fixture with a tenancy section (off mode, its secrets file at perm) and create_dbs.
func tenancyFixture(t *testing.T, perm os.FileMode, section string) string {
	t.Helper()
	cfg := fixture(t, nil, `,"create_dbs":true,"tenancy":`+section)
	sec, _ := json.Marshal(TenancySecrets{AdminToken: "admin-token-0123456789", AppTokens: map[string]string{"notes": appToken}})
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg), "tenancy.json"), sec, perm); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestTenancyConfigRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		cfg, stderr string
	}{
		"secrets readable by others": {tenancyFixture(t, 0o644, `{"mode":"off","secrets_file":"tenancy.json"}`), "make it 0600"},
		"unknown mode":               {tenancyFixture(t, 0o600, `{"mode":"better-auth","secrets_file":"tenancy.json"}`), "want off, api-key or session"},
		"no secrets file":            {tenancyFixture(t, 0o600, `{"mode":"off"}`), "tenancy.secrets_file is required"},
		"api-key without pg_dsn":     {tenancyFixture(t, 0o600, `{"mode":"api-key","secrets_file":"tenancy.json"}`), "need pg_dsn"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr syncBuf
			if code := realMain(context.Background(), []string{"-config", c.cfg}, &stderr); code != exitError || !strings.Contains(stderr.String(), c.stderr) {
				t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
			}
		})
	}
}

// Off mode end to end: the app token reaches default:<app>, created on first use (the first request then meets
// an unregistered schema); another token is AuthError; the admin gate answers without a store.
func TestTenancyOffMode(t *testing.T) {
	cfg := tenancyFixture(t, 0o600, `{"mode":"off","secrets_file":"tenancy.json"}`)
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	waitReady(t, addr)
	post := func(path, token, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	body := `{"v":1,"db":{"tenant":"forged","app":"notes"},"kind":"query","schema":"sha256:` + strings.Repeat("0", 64) +
		`","ops":[{"op":"get","table":"notes","id":"x"}]}`
	if code, out := post("/v1/ops", "wrong-token-0123456789", body); code != 401 || !strings.Contains(out, `"tag":"AuthError"`) {
		t.Fatalf("wrong token: %d %s", code, out)
	}
	if code, out := post("/v1/ops", appToken, body); code != 409 || !strings.Contains(out, "SchemaDriftError") {
		t.Fatalf("app token: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "dbs", "default:notes.db")); err != nil {
		t.Fatalf("the credential's database was not created: %v", err)
	}
	if code, _ := post("/admin/tenants", "", "{}"); code != 401 {
		t.Fatalf("admin without a token: %d", code)
	}
	if code, _ := post("/admin/tenants", "admin-token-0123456789", "{}"); code != 404 {
		t.Fatalf("off mode has no tenant routes: %d", code)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}
}
