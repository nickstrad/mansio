package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestImportUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := realMain(context.Background(), []string{"import-mansio"}, &out, &errb); code != exitUsage {
		t.Fatalf("no -secrets: exit %d", code)
	}
	if code := realMain(context.Background(), []string{"import-mansio", "-secrets", filepath.Join(t.TempDir(), "none")}, &out, &errb); code != exitError {
		t.Fatalf("missing secrets file: exit %d", code)
	}
}

// TestImportGate runs import-mansio against Postgres: a Deno platform schema with one tenant and one key into a
// fresh target schema, a dry run first. Gated on MANSIO_PG_URL.
func TestImportGate(t *testing.T) {
	dsn := os.Getenv("MANSIO_PG_URL")
	if dsn == "" {
		t.Skip("MANSIO_PG_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	b := make([]byte, 6)
	rand.Read(b)
	src, dst := "src_"+hex.EncodeToString(b), "dst_"+hex.EncodeToString(b)
	for _, s := range []string{src, dst} {
		if _, err := conn.Exec(ctx, "create schema "+s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Exec(context.Background(), "drop schema "+s+" cascade") })
	}
	base, err := os.ReadFile("../../internal/placement/pg/testdata/mansio/001_baseline.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "set search_path = "+src+"; "+string(base)+
		"; insert into tenants (id, slug, name) values ('11111111-1111-4111-8111-111111111111', 'acme', 'Acme');"+
		" insert into api_keys (id, tenant_id, key_hash) values ('33333333-3333-4333-8333-333333333333', '11111111-1111-4111-8111-111111111111', '"+
		strings.Repeat("ab", 32)+"'); set search_path = public"); err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(map[string]string{"pg_dsn": dsn, "control_token": strings.Repeat("c", 16), "admin_token": strings.Repeat("a", 16),
		"s3_access_key": "k", "s3_secret_key": "s"})
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, sec, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(extra ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		args := append([]string{"import-mansio", "-secrets", path, "-schema", dst, "-from-schema", src}, extra...)
		if code := realMain(ctx, args, &out, &errb); code != exitOK {
			t.Fatalf("%v: exit %d: %s", extra, code, errb.String())
		}
		return out.String()
	}
	if out := run("-dry-run"); !strings.Contains(out, "would import") || !strings.Contains(out, "1 tenants, 1 api keys") {
		t.Fatalf("dry run: %s", out)
	}
	if out := run(); !strings.HasPrefix(out, "imported: 1 tenants, 1 api keys, 0 users") || !strings.Contains(out, "no Better Auth tables") {
		t.Fatalf("import: %s", out)
	}
	if out := run(); !strings.HasPrefix(out, "imported: 0 tenants, 0 api keys") {
		t.Fatalf("re-run: %s", out)
	}
}
