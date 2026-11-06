package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/nickstrad/mansio/server/internal/metrics"
	"github.com/nickstrad/mansio/server/internal/schemareg"
	"github.com/nickstrad/mansio/server/internal/schemareg/schemaregtest"
)

// The schema registry client as agentd builds it (schemas_url, the control token) and its metrics.
func TestSchemaClientWiring(t *testing.T) {
	if c, err := openSchemas(&Config{}, nil, &Secrets{}); c != nil || err != nil {
		t.Fatalf("without schemas_url: %v, %v", c, err)
	}
	s := schemaregtest.New(t)
	fp := s.Add("tic-tac-toe", tinySchema)
	cfg := defaults()
	cfg.SchemasURL = s.URL
	c, err := openSchemas(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &Secrets{ControlToken: schemaregtest.Token})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, err := c.Get(context.Background(), "tic-tac-toe", fp); err != nil || got != tinySchema {
			t.Fatalf("Get = %q, %v", got, err)
		}
	}
	if _, err := c.Get(context.Background(), "tic-tac-toe", schemareg.Fingerprint([]byte("{}"))); !errors.Is(err, schemareg.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	reg := metrics.NewRegistry()
	schemaMetrics(reg, c)
	text := scrape(t, reg)
	for series, want := range map[string]float64{
		"agentd_schema_cache_hits_total": 1, "agentd_schema_fetches_total": 2, "agentd_schema_fetch_failures_total": 0,
		"agentd_schema_fetch_shed_total": 0, "agentd_schema_cached": 1, "agentd_schema_cached_bytes": float64(len(tinySchema)),
	} {
		if got := sample(t, text, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	cfg.ControlCAFile = "/nonexistent/ca.pem"
	if _, err := openSchemas(&cfg, nil, &Secrets{}); err == nil {
		t.Fatal("a missing CA file must fail")
	}
}

// tinySchema is a valid schema in fingerprint form.
const tinySchema = `{"t":{"fields":{},"storage":"columns","indexes":[]}}`
