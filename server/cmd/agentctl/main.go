// Command agentctl is the scriptable client of agentd's admin endpoints (cmd/agentd/admin.go; Release It C4,
// ch. 14 "CLI first", plan RI-16). It sends one control to the admin listener and prints the JSON answer.
//
//	agentctl [-addr 127.0.0.1:7072] [-timeout 10s] breaker <dep> trip|reset
//	agentctl [flags] fetch-limit <n>
//	agentctl [flags] drain
//	agentctl [flags] passivate <key>
//	agentctl [flags] loglevel debug|info|warn
//
// -addr defaults to $AGENTCTL_ADDR, else 127.0.0.1:7072. The answer goes to stdout as one JSON line. Exit
// codes: 0 the answer is ok, 1 any other answer or no answer (the message on stderr), 2 bad usage,
// 130 interrupted.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nickstrad/ts-db-go/internal/front"
)

const exitOK, exitError, exitUsage, exitInterrupted = 0, 1, 2, 130

// maxAnswer bounds the answer read (admin answers are a few hundred bytes).
const maxAnswer = 64 << 10

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage: agentctl [flags] <command>
commands:
  breaker <dep> trip|reset   force a breaker open until reset, or close it (dep: s3, placement)
  fetch-limit <n>            S3 fetches in flight from now on (n >= 1)
  drain                      stop taking queries (one way; /readyz says draining)
  passivate <key>            write one warm database back and drop its local copy
  loglevel debug|info|warn   change the log level
flags:`

// realMain sends one control and returns the exit code.
func realMain(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage); fs.PrintDefaults() }
	defAddr := getenv("AGENTCTL_ADDR")
	if defAddr == "" {
		defAddr = "127.0.0.1:7072"
	}
	addr := fs.String("addr", defAddr, "agentd's admin listener host:port (admin_listen)")
	timeout := fs.Duration("timeout", 10*time.Second, "give up after this long; also sent to agentd as X-Timeout-Ms")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	path, err := command(fs.Args())
	if err == nil && *timeout <= 0 {
		err = errors.New("-timeout must be > 0")
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentctl: %v\n", err)
		fs.Usage()
		return exitUsage
	}
	answer, err := call(ctx, "http://"+*addr+path, *timeout)
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "agentctl: interrupted")
		return exitInterrupted
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentctl: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, string(answer.raw))
	if answer.Code != "ok" {
		fmt.Fprintf(stderr, "agentctl: %s: %s\n", answer.Code, answer.Message)
		return exitError
	}
	return exitOK
}

// command validates the arguments and returns the endpoint's path and query.
func command(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no command")
	}
	want := func(n int) error {
		if len(args) != n+1 {
			return fmt.Errorf("%s takes %d argument(s), got %d", args[0], n, len(args)-1)
		}
		return nil
	}
	switch args[0] {
	case "breaker":
		if err := want(2); err != nil {
			return "", err
		}
		if args[2] != "trip" && args[2] != "reset" {
			return "", fmt.Errorf("breaker %s %s: want trip or reset", args[1], args[2])
		}
		return "/admin/breaker/" + url.PathEscape(args[1]) + ":" + args[2], nil
	case "fetch-limit":
		if err := want(1); err != nil {
			return "", err
		}
		if n, err := strconv.Atoi(args[1]); err != nil || n < 1 {
			return "", fmt.Errorf("fetch-limit %q: want an integer >= 1", args[1])
		}
		return "/admin/fetch:limit?n=" + args[1], nil
	case "drain":
		return "/admin/drain", want(0)
	case "passivate":
		if err := want(1); err != nil {
			return "", err
		}
		return "/admin/passivate?key=" + url.QueryEscape(args[1]), nil
	case "loglevel":
		if err := want(1); err != nil {
			return "", err
		}
		return "/admin/loglevel?level=" + url.QueryEscape(args[1]), nil
	}
	return "", fmt.Errorf("unknown command %q", args[0])
}

// answer is agentd's JSON answer: code (and message unless ok), plus the raw bytes to print.
type answer struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	raw     []byte
}

// call POSTs to u within timeout and decodes the answer. A non-JSON answer is an error.
func call(ctx context.Context, u string, timeout time.Duration) (answer, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return answer{}, err
	}
	req.Header.Set(front.TimeoutHeader, strconv.FormatInt(timeout.Milliseconds(), 10))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{}, fmt.Errorf("no answer from %s: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	if err != nil {
		return answer{}, fmt.Errorf("answer from %s cut off: %w", req.URL.Host, err)
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil || a.Code == "" {
		return answer{}, fmt.Errorf("%s answered %s with no JSON code: %q", req.URL.Host, res.Status, bytes.TrimSpace(raw))
	}
	a.raw = bytes.TrimSpace(raw)
	return a, nil
}
