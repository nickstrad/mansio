package pg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

// Postgres tests are gated on MANSIO_PG_URL (deploy/postgres/docker-compose.yml) and each runs in a fresh
// schema that is dropped afterwards (knowledge/testing/README.md, SQL G-21).

var ctx = context.Background()

func pgURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("MANSIO_PG_URL")
	if u == "" {
		t.Skip("MANSIO_PG_URL not set (docker compose -f deploy/postgres/docker-compose.yml up -d)")
	}
	return u
}

// adminConn is a direct connection outside any adapter, for setup and for holding locks.
func adminConn(t testing.TB, schema string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(pgURL(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "test/admin"
	if schema != "" {
		cfg.RuntimeParams["search_path"] = schema
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// freshSchema creates an empty schema and drops it when the test ends.
func freshSchema(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	c := adminConn(t, "")
	if _, err := c.Exec(ctx, "create schema "+name); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := c.Exec(context.Background(), "drop schema "+name+" cascade"); err != nil {
			t.Errorf("drop schema %s: %v", name, err)
		}
	})
	return name
}

// testVMs are registered in every schema newPlacement makes: the contract suite's and the owner harness's.
func testVMs() []string { return append(ownertest.SuiteVMs(), "a", "b") }

// newPlacement opens an adapter on a fresh, migrated schema with testVMs registered. mod may change the
// config (e.g. point the DSN at a faultnet proxy).
func newPlacement(t testing.TB, mod func(*Config)) (*Placement, string) {
	t.Helper()
	schema := freshSchema(t)
	return openOn(t, schema, mod), schema
}

// openOn opens another adapter on an existing schema, migrating it and registering testVMs (idempotent).
func openOn(t testing.TB, schema string, mod func(*Config)) *Placement {
	t.Helper()
	p := open(t, pgURL(t), schema, mod)
	if _, err := p.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, vm := range testVMs() {
		if err := p.RegisterVM(ctx, vm, vm+":7000"); err != nil {
			t.Fatalf("RegisterVM(%s): %v", vm, err)
		}
	}
	return p
}

// open opens an adapter on dsn and schema, closed when the test ends.
func open(t testing.TB, dsn, schema string, mod func(*Config)) *Placement {
	t.Helper()
	cfg := Config{DSN: dsn, Schema: schema, AppName: "test/placement"}
	if mod != nil {
		mod(&cfg)
	}
	p, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// pgAddr parses MANSIO_PG_URL (it must be a URL) and returns it with its host:port.
func pgAddr(t testing.TB) (*url.URL, string) {
	t.Helper()
	u, err := url.Parse(pgURL(t))
	if err != nil || u.Host == "" {
		t.Fatalf("MANSIO_PG_URL must be a postgres:// URL for proxy tests: %v", err)
	}
	if u.Port() == "" {
		return u, u.Host + ":5432"
	}
	return u, u.Host
}
