package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRealMainExitCodes(t *testing.T) {
	good := writeConfig(t, `{"name":"t","sizes":[{"size":"64KiB","count":2}],"out_dir":"`+t.TempDir()+`"}`)
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{"unknown command", []string{"-config", good, "fly"}, exitUsage, "usage:"},
		{"extra argument", []string{"-config", good, "plan", "x"}, exitUsage, "usage:"},
		{"bad flag", []string{"-nope"}, exitUsage, ""},
		{"plan", []string{"-config", good, "plan"}, exitOK, `coldload "t"`},
		{"missing config file", []string{"-config", "/no/such.json", "plan"}, exitError, "cannot read config"},
		{"unknown config field", []string{"-config", writeConfig(t, `{"name":"t","sizez":[]}`), "plan"}, exitError, "cannot parse config"},
		{"invalid config", []string{"-config", writeConfig(t, `{"name":"t","sizes":[{"size":"0","count":1}]}`), "plan"}, exitError, "bad size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := realMain(context.Background(), tc.args, &stderr)
			if code != tc.wantCode || !strings.Contains(stderr.String(), tc.wantOut) {
				t.Fatalf("code %d, stderr %q; want %d and %q", code, stderr.String(), tc.wantCode, tc.wantOut)
			}
			if tc.wantCode == exitError && !strings.Contains(stderr.String(), "[log ID ") {
				t.Fatalf("expected failure without a log ID: %q", stderr.String())
			}
		})
	}
}

func TestRealMainStoreDownIsAnExpectedErrorWithLogID(t *testing.T) {
	out := t.TempDir()
	cfg := writeConfig(t, `{"name":"t","s3":{"endpoint":"127.0.0.1:1"},"sizes":[{"size":"64KiB","count":1}],"out_dir":"`+out+`"}`)
	var stderr bytes.Buffer
	code := realMain(context.Background(), []string{"-config", cfg, "run"}, &stderr)
	if code != exitError || !strings.Contains(stderr.String(), "is SeaweedFS up?") {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	log, _ := os.ReadFile(filepath.Join(out, "coldload.log"))
	id := stderr.String()[strings.Index(stderr.String(), "[log ID ")+8:]
	id = id[:strings.Index(id, "]")]
	if !strings.Contains(string(log), `"log_id":"`+id+`"`) || !strings.Contains(string(log), `"chain"`) {
		t.Fatalf("log ID %s not found with its error chain in coldload.log:\n%s", id, log)
	}
}
