package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const tiny = `{"name":"tiny","workers":8,"sample_every":"50ms",
	"schema":{"min_tables":1,"max_tables":2,"min_rows":5,"max_rows":10},
	"phases":[{"dbs":2,"duration":"150ms","start_qps":200}]}`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, ctx context.Context, args ...string) (int, string) {
	t.Helper()
	var stderr bytes.Buffer
	code := realMain(ctx, args, &stderr)
	return code, stderr.String()
}

var logID = regexp.MustCompile(`log ID ([0-9a-f]{8})`)

func TestExitCodes(t *testing.T) {
	cfg := writeConfig(t, tiny)
	out := func() string { return filepath.Join(t.TempDir(), "out") }
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name   string
		ctx    context.Context
		args   []string
		code   int
		stderr string
	}{
		{"unknown flag", context.Background(), []string{"-nope"}, exitUsage, "flag provided but not defined"},
		{"stray argument", context.Background(), []string{"-config", cfg, "extra"}, exitUsage, "unexpected arguments"},
		{"missing config", context.Background(), []string{"-config", "/no/such.json"}, exitError, "error: "},
		{"invalid config", context.Background(), []string{"-config", writeConfig(t, `{"phases":[]}`)}, exitError, "at least one phase"},
		{"dry run", context.Background(), []string{"-config", cfg, "-dry-run", "-out", out()}, exitOK, "phase 0"},
		{"run", context.Background(), []string{"-config", cfg, "-out", out(), "-db-dir", filepath.Join(t.TempDir(), "d")}, exitOK, "msg=done"},
		{"interrupted", cancelled, []string{"-config", cfg, "-out", out(), "-db-dir", filepath.Join(t.TempDir(), "d")}, exitInterrupted, "interrupted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stderr := run(t, tc.ctx, tc.args...)
			if code != tc.code || !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("exit %d (want %d), stderr:\n%s\nwant it to contain %q", code, tc.code, stderr, tc.stderr)
			}
			if strings.Count(stderr, "error: ") > 1 {
				t.Fatalf("an error was reported more than once:\n%s", stderr)
			}
			if strings.Contains(stderr, "goroutine ") {
				t.Fatalf("a stack reached the user:\n%s", stderr)
			}
		})
	}
}

// A failure after the log file exists: the user sees one line with a log ID; the log has that ID with the
// error chain and the stack.
func TestFailureLogIDIsInTheLog(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	code, stderr := run(t, context.Background(), "-config", writeConfig(t, tiny), "-out", out, "-db-dir", filepath.Join(blocker, "dbs"))
	if code != exitError {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	m := logID.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("no log ID in:\n%s", stderr)
	}
	b, err := os.ReadFile(filepath.Join(out, "sqlbench.log"))
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		if rec["log_id"] == m[1] {
			found = rec
		}
	}
	if found == nil || found["chain"] == nil || found["stack"] == nil {
		t.Fatalf("log ID %s not logged with chain and stack:\n%s", m[1], b)
	}
	if ts, _ := found["time"].(string); !strings.HasSuffix(ts, "Z") {
		t.Fatalf("log time %q is not UTC", ts)
	}
}

func TestInterruptMidRunFinishesFiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	long := strings.Replace(tiny, `"150ms"`, `"1h"`, 1)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	code, stderr := run(t, ctx, "-config", writeConfig(t, long), "-out", out, "-db-dir", filepath.Join(t.TempDir(), "d"))
	if code != exitInterrupted {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	b, _ := os.ReadFile(filepath.Join(out, "requests.csv"))
	if lines := bytes.Count(b, []byte("\n")); lines < 2 {
		t.Fatalf("requests.csv has %d lines after an interrupt", lines)
	}
}

// An error that no module made well-formed is a bug: exit 70, a log ID, no details for the user.
func TestMalformedErrorIsABug(t *testing.T) {
	var stderr, log bytes.Buffer
	rep := reporter{detail: slog.New(slog.NewJSONHandler(&log, nil)), stderr: &stderr, logs: "x.log"}
	if code := rep.report(errors.New("raw")); code != exitBug {
		t.Fatalf("exit %d, want %d", code, exitBug)
	}
	m := logID.FindStringSubmatch(stderr.String())
	if m == nil || !strings.Contains(stderr.String(), "This is a bug") || strings.Contains(stderr.String(), "raw") {
		t.Fatalf("stderr: %s", stderr.String())
	}
	if !strings.Contains(log.String(), m[1]) {
		t.Fatalf("log ID %s not in the log: %s", m[1], log.String())
	}
}

// Before the log file exists there is no ID to look up: the details go to stderr instead.
func TestBugBeforeTheLogFileShowsDetails(t *testing.T) {
	var stderr bytes.Buffer
	if code := (reporter{stderr: &stderr}).bug(errors.New("raw"), "stack here"); code != exitBug {
		t.Fatalf("exit %d", code)
	}
	if out := stderr.String(); !strings.Contains(out, "raw") || !strings.Contains(out, "stack here") || logID.MatchString(out) {
		t.Fatalf("stderr: %s", out)
	}
}
