package logx

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/errs"
)

func TestReporter(t *testing.T) {
	var stderr, logged bytes.Buffer
	r := Reporter{Stderr: &stderr, Name: "svc", Known: map[string]bool{"svc": true}}
	if code := r.Bug(errors.New("boom"), "stack"); code != ExitBug || !strings.Contains(stderr.String(), "stack") {
		t.Fatalf("bug before the log: %d %q", code, stderr.String())
	}
	r.Log = New(&logged, nil)
	stderr.Reset()
	if code := r.Report(errs.Wrap("svc", nil, "cannot start")); code != ExitError || !strings.Contains(stderr.String(), "cannot start [log ID ") ||
		!strings.Contains(logged.String(), `"msg":"svc failed"`) {
		t.Fatalf("expected failure: %d %q %q", code, stderr.String(), logged.String())
	}
	if code := r.Report(errs.Wrap("other", nil, "x")); code != ExitBug || !strings.Contains(logged.String(), "unexpected error (bug)") {
		t.Fatalf("unknown module: %d", code)
	}
	log, closeLog, err := Open(filepath.Join(t.TempDir(), "sub", "svc.log"), Levels["warn"], &stderr)
	if err != nil {
		t.Fatal(err)
	}
	log.Warn("x")
	closeLog()
}
