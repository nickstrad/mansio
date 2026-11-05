// Command controld is the control plane (control-plane plan CP-4): the registries (tenants, databases, VMs
// and their heartbeats) and the database lifecycle (create, move, suspend/resume, drop, and the M5 restore
// of a VM an operator attested dead) over the Postgres placement authority. It reaches agents only through
// their lifecycle endpoints, each with a deadline and its own breaker, and finishes stuck flows from table
// scans (internal/control).
//
//	controld -config controld.json [-addr-file path] [-dev]   serve (-dev: plain-HTTP S3 and listener allowed)
//	controld <command> -addr URL -secrets file [-ca file] ...  one admin call (prints the JSON answer)
//
// The listener speaks HTTPS with a tls section (cert_file, key_file); without one it is refused unless -dev.
//
// Commands: create -tenant T -name N | get -db K | move -db K [-to VM] | suspend -db K | resume -db K [-to VM]
// | drop -db K | attest-dead -vm VM -by WHO | restore -vm VM | vms | create-key -tenant T -principal P [-perms P]
// | revoke-key -id ID.
//
// The locator and ticket issuer (CP-5, locator.go) serve on the same listener: POST /v1/resolve (an API key)
// and GET /v1/keys (the ticket public keys agents fetch).
//
// Exit codes: 0 ok, 1 failed (an error answer, or a startup error with its log ID), 2 usage, 70 bug,
// 130 interrupted before serving.
package main

import (
	"context"
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
	if len(args) > 0 && commands[args[0]] != nil {
		return runCommand(ctx, args[0], args[1:], stdout, stderr)
	}
	return serveMain(ctx, args, stderr)
}
