//go:build e2e

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// End to end: the real binary, a shipped config scaled down, and the files a user would analyze.
// Run with: go test -tags e2e ./cmd/bench/sqlbench

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sqlbench-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "sqlbench")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestBinaryRunsAConfigAndWritesItsFiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	cfg := writeConfig(t, tiny)
	cmd := exec.Command(binary, "-config", cfg, "-out", out, "-db-dir", filepath.Join(t.TempDir(), "d"))
	stderr, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for _, f := range []string{"requests.csv", "samples.csv", "phases.csv", "schemas.csv", "run.json", "sqlbench.log"} {
		if st, err := os.Stat(filepath.Join(out, f)); err != nil || st.Size() == 0 {
			t.Errorf("%s missing or empty", f)
		}
	}
}

func TestBinaryExitCodes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"-nope"}, exitUsage},
		{[]string{"-config", "/no/such.json"}, exitError},
		{[]string{"-config", "../../configs/ramp.json", "-dry-run"}, exitOK},
	} {
		err := exec.Command(binary, tc.args...).Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d", strings.Join(tc.args, " "), code, tc.code)
		}
	}
}
