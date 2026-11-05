package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
)

func TestPhaseDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	end := now.Add(10 * time.Second)
	tests := []struct {
		name string
		max  time.Duration
		want time.Time
	}{
		{"no cap: the rest of the budget", 0, end},
		{"cap inside the budget", 3 * time.Second, now.Add(3 * time.Second)},
		{"cap = the rest (on point)", 10 * time.Second, end},
		{"cap past the budget (off point)", 10*time.Second + 1, end},
	}
	for _, tc := range tests {
		if got := phaseDeadline(now, end, tc.max); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := phaseDeadline(end.Add(time.Second), end, time.Second); !got.Equal(end) {
		t.Errorf("after the budget ended: %v, want the (past) end %v", got, end)
	}
}

// One budget for the whole sequence: a phase that overruns its share leaves less to the next, a failed phase
// does not skip the next, and the total stays within the budget.
func TestShutdownOneBudget(t *testing.T) {
	var log bytes.Buffer
	var ran []string
	block := func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			ran = append(ran, name)
			<-ctx.Done()
			return ctx.Err()
		}
	}
	t0 := time.Now()
	err := shutdown(slog.New(slog.NewJSONHandler(&log, nil)), 300*time.Millisecond, []phase{
		{"drain", 100 * time.Millisecond, block("drain")},
		{"close", 0, block("close")},
		{"after", 0, func(ctx context.Context) error {
			ran = append(ran, "after")
			if ctx.Err() == nil {
				t.Error("a phase after the budget ended got a live ctx")
			}
			return nil
		}},
	})
	took := time.Since(t0)
	if took > 450*time.Millisecond || took < 300*time.Millisecond {
		t.Fatalf("took %v, budget 300ms", took)
	}
	if strings.Join(ran, ",") != "drain,close,after" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ran %v, err %v", ran, err)
	}
	for _, ev := range []string{"AGENT.PHASE_FAILED", "AGENT.PHASE_DONE", "AGENT.SHUTDOWN_DONE"} {
		if !strings.Contains(log.String(), ev) {
			t.Errorf("log lacks %s:\n%s", ev, log.String())
		}
	}
}

// TestHelperAgentd is not a test: TestSIGTERMMeetsBudget runs the test binary as agentd through it, so the
// real signal path (signal.NotifyContext in main) is exercised.
func TestHelperAgentd(t *testing.T) {
	if os.Getenv("AGENTD_HELPER") != "1" {
		t.Skip("helper process only")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var args []string
	json.Unmarshal([]byte(os.Getenv("AGENTD_ARGS")), &args)
	code := realMain(ctx, args, os.Stderr)
	stop()
	os.Exit(code)
}

// RI-06 e2e: SIGTERM with a request that will not finish. The drain is cut at drain_timeout, the databases
// close with what is left, and the process exits (1, reported) within shutdown_budget.
func TestSIGTERMMeetsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a process")
	}
	for _, tc := range []struct {
		name string
		slow bool
		code int
	}{{"idle", false, exitOK}, {"request outlives the drain", true, exitError}} {
		t.Run(tc.name, func(t *testing.T) {
			const budget = 1500 * time.Millisecond
			cfg := fixture(t, []string{"db-1"}, `,"drain_timeout":"300ms","shutdown_budget":"1500ms","statement_timeout":"20s","default_timeout":"25s"`)
			addrFile := filepath.Join(t.TempDir(), "addr")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAgentd$")
			args, _ := json.Marshal([]string{"-config", cfg, "-addr-file", addrFile})
			cmd.Env = append(os.Environ(), "AGENTD_HELPER=1", "AGENTD_ARGS="+string(args))
			var stderr syncBuf
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			defer cmd.Process.Kill()
			addr := waitAddrProc(t, addrFile, exited)
			if tc.slow {
				b, _ := json.Marshal(map[string]any{"db": "db-1", "kind": "read", "sql": fronttest.SlowSQL})
				req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.QueryPath, bytes.NewReader(b))
				req.Header.Set("Authorization", "Bearer "+iss.Mint(t, iss.Claims("db-1", "vm-1", time.Now(), time.Minute)))
				go http.DefaultClient.Do(req)
				time.Sleep(200 * time.Millisecond) // the slow statement is running
			}
			t0 := time.Now()
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(budget + 3*time.Second):
				t.Fatalf("still running %v after SIGTERM (budget %v); stderr:\n%s", time.Since(t0), budget, stderr.String())
			}
			took := time.Since(t0)
			if code := cmd.ProcessState.ExitCode(); code != tc.code || took > budget+time.Second {
				t.Fatalf("exit %d (want %d) after %v (budget %v); stderr:\n%s", code, tc.code, took, budget, stderr.String())
			}
			logb, _ := os.ReadFile(filepath.Join(filepath.Dir(cfg), "agentd.log"))
			if !strings.Contains(string(logb), "AGENT.SHUTDOWN_DONE") {
				t.Fatalf("log:\n%s", logb)
			}
		})
	}
}

func waitAddrProc(t *testing.T, path string, exited chan error) string {
	t.Helper()
	for range 1000 {
		select {
		case err := <-exited:
			t.Fatalf("agentd exited before serving: %v", err)
		default:
		}
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.Fields(string(b))[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agentd did not start")
	return ""
}

// The drain is part of the shutdown, so drain_timeout may not exceed shutdown_budget.
func TestDrainFitsShutdownBudget(t *testing.T) {
	for _, tc := range []struct {
		drain, budget string
		ok            bool
	}{{"1s", "1s", true}, {"1001ms", "1s", false}, {"10s", "60s", true}} {
		_, err := parseConfig(strings.NewReader(`{"vm":"v","db_dir":"d","keys_file":"k","drain_timeout":"`+tc.drain+`","shutdown_budget":"`+tc.budget+`"}`), "/")
		if (err == nil) != tc.ok {
			t.Errorf("drain %s budget %s: err %v", tc.drain, tc.budget, err)
		}
	}
}
