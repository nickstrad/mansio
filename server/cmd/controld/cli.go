package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/tlsx"
)

// command is one admin call: its flags and the request they make.
type command struct {
	usage string
	build func(fs *flag.FlagSet) func() (method, path string, body any, err error)
}

func need(names ...string) error {
	for _, n := range names {
		if n == "" {
			return fmt.Errorf("missing a required flag")
		}
	}
	return nil
}

// commands are the admin calls, each a thin wrapper over one API endpoint (Release It C4: scriptable).
var commands = map[string]*command{
	"create": {"-tenant T -name N", func(fs *flag.FlagSet) func() (string, string, any, error) {
		tenant, name := fs.String("tenant", "", "tenant id"), fs.String("name", "", "database name")
		return func() (string, string, any, error) {
			return http.MethodPost, "/v1/databases", map[string]string{"tenant": *tenant, "name": *name}, need(*tenant, *name)
		}
	}},
	"get": {"-db KEY", dbCmd(http.MethodGet, "")},
	"move": {"-db KEY [-to VM]", func(fs *flag.FlagSet) func() (string, string, any, error) {
		db, to := fs.String("db", "", "database key (tenant:name)"), fs.String("to", "", "destination vm (default: the policy picks)")
		return func() (string, string, any, error) {
			return http.MethodPost, "/v1/databases/" + url.PathEscape(*db) + "/move", map[string]string{"to": *to}, need(*db)
		}
	}},
	"suspend": {"-db KEY", dbCmd(http.MethodPost, "/suspend")},
	"resume": {"-db KEY [-to VM]", func(fs *flag.FlagSet) func() (string, string, any, error) {
		db, to := fs.String("db", "", "database key (tenant:name)"), fs.String("to", "", "destination vm (default: the policy picks)")
		return func() (string, string, any, error) {
			return http.MethodPost, "/v1/databases/" + url.PathEscape(*db) + "/resume", map[string]string{"to": *to}, need(*db)
		}
	}},
	"drop": {"-db KEY", dbCmd(http.MethodPost, "/drop")},
	"attest-dead": {"-vm VM -by WHO", func(fs *flag.FlagSet) func() (string, string, any, error) {
		vm, by := fs.String("vm", "", "the vm an operator confirmed dead (M5)"), fs.String("by", "", "who attests it")
		return func() (string, string, any, error) {
			return http.MethodPost, "/v1/vms/" + url.PathEscape(*vm) + "/attest-dead", map[string]string{"by": *by}, need(*vm, *by)
		}
	}},
	"restore": {"-vm VM", func(fs *flag.FlagSet) func() (string, string, any, error) {
		vm := fs.String("vm", "", "a vm attested dead: move its databases elsewhere")
		return func() (string, string, any, error) {
			return http.MethodPost, "/v1/vms/" + url.PathEscape(*vm) + "/restore", nil, need(*vm)
		}
	}},
	"create-key": {"-tenant T -principal P [-perms read|write|read|write]", func(fs *flag.FlagSet) func() (string, string, any, error) {
		tenant, principal := fs.String("tenant", "", "tenant id"), fs.String("principal", "", "who the key is for (the tickets' sub)")
		perms := fs.String("perms", "read|write", "read, write or read|write")
		return func() (string, string, any, error) {
			return http.MethodPost, "/v1/tenants/" + url.PathEscape(*tenant) + "/api-keys", map[string]string{"principal": *principal, "perms": *perms},
				need(*tenant, *principal)
		}
	}},
	"revoke-key": {"-id KEY_ID", func(fs *flag.FlagSet) func() (string, string, any, error) {
		id := fs.String("id", "", "the key's id (the 16 hex digits after tsdb_)")
		return func() (string, string, any, error) {
			return http.MethodPost, "/v1/api-keys/" + url.PathEscape(*id) + "/revoke", nil, need(*id)
		}
	}},
	"vms": {"", func(fs *flag.FlagSet) func() (string, string, any, error) {
		return func() (string, string, any, error) { return http.MethodGet, "/v1/vms", nil, nil }
	}},
}

func dbCmd(method, suffix string) func(fs *flag.FlagSet) func() (string, string, any, error) {
	return func(fs *flag.FlagSet) func() (string, string, any, error) {
		db := fs.String("db", "", "database key (tenant:name)")
		return func() (string, string, any, error) {
			return method, "/v1/databases/" + url.PathEscape(*db) + suffix, nil, need(*db)
		}
	}
}

// runCommand makes one admin call and prints the answer. Exit 0 on a 2xx answer, 1 on an error answer or no
// answer, 2 on bad usage.
func runCommand(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	cmd := commands[name]
	fs := flag.NewFlagSet("controld "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "http://127.0.0.1:7080", "controld API URL")
	secrets := fs.String("secrets", "", "controld secrets file (for admin_token)")
	timeout := fs.Duration("timeout", 3*time.Minute, "call deadline")
	ca := fs.String("ca", "", "PEM CA bundle that signs controld's certificate (an https -addr with a private CA)")
	req := cmd.build(fs)
	usage := func() int {
		fmt.Fprintf(stderr, "usage: controld %s -addr URL -secrets FILE [-ca FILE] %s\n", name, cmd.usage)
		return exitUsage
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *secrets == "" {
		return usage()
	}
	method, path, body, err := req()
	if err != nil {
		return usage()
	}
	sec, err := loadSecrets(*secrets)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	hc, err := tlsx.HTTPClient(*ca)
	if err != nil {
		fmt.Fprintf(stderr, "error: cannot load -ca %s: %v\n", *ca, err)
		return exitError
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(*addr, "/")+path, rd)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}
	r.Header.Set("Authorization", "Bearer "+sec.AdminToken)
	res, err := hc.Do(r)
	if err != nil {
		fmt.Fprintf(stderr, "error: no answer from %s: %v\n", *addr, err)
		return exitError
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	stdout.Write(out)
	if res.StatusCode/100 != 2 {
		return exitError
	}
	return exitOK
}
