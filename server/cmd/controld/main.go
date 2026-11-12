// Command controld is the control plane (control-plane plan CP-4): the registries (tenants, databases, VMs
// and their heartbeats) and the database lifecycle (create, move, suspend/resume, drop, and the M5 restore
// of a VM an operator attested dead) over the Postgres placement authority. It reaches agents only through
// their lifecycle endpoints, each with a deadline and its own breaker, and finishes stuck flows from table
// scans (internal/control).
//
//	controld -config controld.json [-addr-file path] [-dev]   serve (-dev: plain-HTTP S3 and listener allowed)
//	controld keygen -dir DIR [-public FILE]                    seed an empty ticket key ring (keygen.go)
//	controld import-mansio -secrets FILE ...                   copy the Deno platform's tenancy into Postgres (import.go)
//
// The listener speaks HTTPS with a tls section (cert_file, key_file); without one it is refused unless -dev.
//
// The admin calls to a running controld (create, move, attest-dead, ...) are mansioctl's (cmd/mansioctl);
// controld <old command> says which mansioctl command replaced it and exits 2.
//
// The locator and ticket issuer (CP-5, locator.go) serve on the same listener: POST /v1/resolve (an API key)
// and GET /v1/keys (the ticket public keys agents fetch).
//
// Exit codes: 0 ok, 1 failed (an error answer, or a startup error with its log ID), 2 usage, 70 bug,
// 130 interrupted before serving.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const (
	exitOK, exitError, exitUsage, exitBug, exitInterrupted = 0, 1, 2, 70, 130
	module                                                 = "controld"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "keygen" {
		return keygenMain(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "import-mansio" {
		return importMain(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && movedCommands[args[0]] != "" {
		fmt.Fprintf(stderr, "controld %s moved to mansioctl: run mansioctl -addr URL -secrets FILE [-ca FILE] %s ...\n", args[0], movedCommands[args[0]])
		return exitUsage
	}
	return serveMain(ctx, args, stderr)
}

// movedCommands are the admin calls controld used to make itself, and their mansioctl commands now.
var movedCommands = map[string]string{
	"create": "databases create", "get": "databases get", "move": "databases move", "suspend": "databases suspend",
	"resume": "databases resume", "drop": "databases drop", "vms": "vms list", "attest-dead": "vms attest-dead",
	"restore": "vms restore", "create-key": "keys create", "revoke-key": "keys revoke", "limit": "limits set",
}
