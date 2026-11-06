package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

func run(t *testing.T, ctx context.Context, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := realMain(ctx, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestExitCodes(t *testing.T) {
	bg := context.Background()
	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{nil, exitUsage, "usage"},
		{[]string{"nope"}, exitUsage, "usage"},
		{[]string{"keygen"}, exitUsage, "-out is required"},
		{[]string{"seed", "-dbs", "0", "-dir", "x"}, exitUsage, "need -dir"},
		{[]string{"run", "-conc", "0"}, exitUsage, "need dbs"},
		{[]string{"run", "-mode", "http"}, exitUsage, ""},
		{[]string{"run", "-mode", "inproc", "-dir", t.TempDir()}, exitError, "run frontbench seed first"},
		{[]string{"run", "-mode", "http", "-key", "/no/such"}, exitError, "cannot read issuer key"},
		{[]string{"run", "-extra-flag"}, exitUsage, "flag provided but not defined"},
		{[]string{"seed", "-dir", t.TempDir(), "stray"}, exitUsage, "unexpected arguments"},
	} {
		code, _, stderr := run(t, bg, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.msg) {
			t.Errorf("%v: exit %d (want %d), stderr %q", tc.args, code, tc.code, stderr)
		}
	}
}

// keygen → seed → run in every mode against an in-process agent front; the http run uses the keygen key.
func TestKeygenSeedAndRunEveryMode(t *testing.T) {
	dir := t.TempDir()
	bg := context.Background()
	if code, _, stderr := run(t, bg, "keygen", "-out", filepath.Join(dir, "keys")); code != exitOK {
		t.Fatal(stderr)
	}
	if code, _, stderr := run(t, bg, "seed", "-dir", filepath.Join(dir, "dbs"), "-dbs", "3"); code != exitOK {
		t.Fatal(stderr)
	}
	short := []string{"-dbs", "3", "-conc", "4", "-duration", "200ms", "-warmup", "50ms", "-write", "0.3"}
	for _, mode := range []string{"inproc", "pipeline"} {
		out := filepath.Join(dir, mode+".json")
		code, stdout, stderr := run(t, bg, append([]string{"run", "-mode", mode, "-dir", filepath.Join(dir, "dbs"), "-out", out}, short...)...)
		if code != exitOK || !strings.Contains(stdout, "ok/s") {
			t.Fatalf("%s: exit %d\n%s%s", mode, code, stdout, stderr)
		}
		var s Summary
		b, _ := os.ReadFile(out)
		if json.Unmarshal(b, &s) != nil || s.Codes["ok"] == 0 || s.Requests != s.Codes["ok"] {
			t.Fatalf("%s summary %s", mode, b)
		}
	}

	// An in-process agent: real actors, the keygen key set, the HTTP handler.
	f, _ := os.Open(filepath.Join(dir, "keys", "keys.json"))
	keys, err := ticket.ParsePublicKeys(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	host, err := openActors(runFlags{dir: filepath.Join(dir, "dbs"), dbs: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	srv := front.NewServer(front.Config{}, ticket.Verifier{Keys: ticket.NewKeyRing(keys), VM: "vm-local", Skew: time.Second},
		front.StaticAdmitter{"db-0": 1, "db-1": 1, "db-2": 1}, front.ActorExecutor{Host: host}, nil)
	hs := httptest.NewServer(front.NewHandler(srv, slog.New(slog.DiscardHandler)))
	defer hs.Close()
	code, stdout, stderr := run(t, bg, append([]string{"run", "-mode", "http", "-url", hs.URL, "-key", filepath.Join(dir, "keys", "issuer.key")}, short...)...)
	if code != exitOK || !strings.Contains(stdout, "http/h1") {
		t.Fatalf("http: exit %d\n%s%s", code, stdout, stderr)
	}
}

func TestInterrupted(t *testing.T) {
	dir := t.TempDir()
	if code, _, stderr := run(t, context.Background(), "seed", "-dir", dir, "-dbs", "1"); code != exitOK {
		t.Fatal(stderr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	code, _, stderr := run(t, ctx, "run", "-mode", "inproc", "-dir", dir, "-dbs", "1", "-conc", "2", "-duration", "1h")
	if code != exitInterrupted {
		t.Fatalf("exit %d %s", code, stderr)
	}
}
