// Command mansioctl is the platform's admin CLI: it calls the admin API (/admin/…) of whichever process
// serves it (agentd in the api-key and session tenancy modes, controld in a multi-node deployment), makes one
// change or reads one listing, and prints the answer.
//
//	mansioctl [global flags] <group> <command> [flags] [args]
//	mansioctl tenants create <name> [-slug S]       a tenant and its first API key (printed once)
//	mansioctl tenants list | show <id> | deactivate <id>
//	mansioctl tenants issue-key <id> [-name N]      a new API key (printed once)
//	mansioctl tenants revoke-key <key-id>
//
// Commands come in groups. A group is one file (tenants.go) that defines a *group: a map of command name to
// a usage line and a parse function that checks the arguments and returns the action. The groups map below
// is the registry: adding a group is adding its file and one entry there.
//
// The admin token comes from -token-file or $ADMIN_TOKEN, never from a flag: a command line is visible to
// other users of the machine and kept in shell history. -addr defaults to $MANSIOCTL_ADDR, else
// http://127.0.0.1:7070. Nothing is stored locally; a fresh API key appears only in the output of the command
// that made it. The answer goes to stdout as one JSON document (-format json, the default) or as a table
// (-format table). Exit codes: 0 ok, 1 an error answer or no answer (the message on stderr), 2 bad usage,
// 130 interrupted.
package main

import (
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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nickstrad/mansio/server/internal/tlsx"
)

const exitOK, exitError, exitUsage, exitInterrupted = 0, 1, 2, 130

// defaultAddr is agentd's query listener in the sample configs.
const defaultAddr = "http://127.0.0.1:7070"

// groups is the registry of command groups (mansioctl <group> <command>).
var groups = map[string]*group{
	"tenants": tenantsGroup,
}

// group is a set of related commands.
type group struct {
	summary  string
	commands map[string]*command
}

// command is one admin command: its usage after "mansioctl <group> <name>", a one-line summary, and parse,
// which registers the command's flags on fs and returns a function that checks the positional arguments (after
// the flags are parsed) and returns the action. An error from that function is a usage error (exit 2).
type command struct {
	usage, summary string
	parse          func(fs *flag.FlagSet) func(args []string) (action, error)
}

// action makes the command's calls with c and returns what to print.
type action func(ctx context.Context, c *client) (view, error)

// view is a command's answer: the JSON document (-format json) and the table (-format table). note, if set, is a
// line for the person running the command (e.g. "save this key"): after the table, or on stderr with JSON so
// stdout stays one document.
type view struct {
	json  any
	table string
	note  string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := realMain(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usageHead = `usage: mansioctl [global flags] <group> <command> [flags] [args]

The platform's admin CLI over the admin API (/admin/...), served by agentd (tenancy mode api-key or session)
or by controld (multi-node).

The admin token comes from -token-file or the ADMIN_TOKEN environment variable, never from a flag: a command
line is visible to other users of the machine (ps) and stays in shell history.
`

// realMain runs one command and returns the exit code.
func realMain(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mansioctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	defAddr := getenv("MANSIOCTL_ADDR")
	if defAddr == "" {
		defAddr = defaultAddr
	}
	addr := fs.String("addr", defAddr, "the admin API's base URL, http or https (default $MANSIOCTL_ADDR, else "+defaultAddr+")")
	tokenFile := fs.String("token-file", "", "file holding the admin token (mode 0600): the token alone, or a JSON secrets file with admin_token (default: $ADMIN_TOKEN)")
	ca := fs.String("ca", "", "PEM CA bundle that signs the server's certificate (an https -addr with a private CA)")
	timeout := fs.Duration("timeout", 30*time.Second, "deadline for the whole command")
	format := fs.String("format", "json", "output: json (one JSON document) or table")
	fs.Usage = func() { printUsage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	run, err := parseCommand(fs.Args(), stderr)
	if err == nil && *timeout <= 0 {
		err = errors.New("-timeout must be > 0")
	}
	if err == nil && *format != "json" && *format != "table" {
		err = fmt.Errorf("-format %q: want json or table", *format)
	}
	var base *url.URL
	if err == nil {
		base, err = checkAddr(*addr)
	}
	if err != nil {
		if !errors.Is(err, errUsageShown) {
			fmt.Fprintf(stderr, "mansioctl: %v\n", err)
			fmt.Fprintln(stderr, "run mansioctl -h for usage")
		}
		return exitUsage
	}
	token, err := adminToken(*tokenFile, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "mansioctl: %v\n", err)
		return exitUsage
	}
	hc, err := tlsx.HTTPClient(*ca)
	if err != nil {
		fmt.Fprintf(stderr, "mansioctl: cannot load -ca %s: %v\n", *ca, err)
		return exitError
	}
	// A redirect is answered as an error rather than followed: the token goes only to -addr.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	cctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	v, err := run(cctx, &client{hc: hc, base: base, token: token})
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "mansioctl: interrupted")
		return exitInterrupted
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w (no answer within -timeout %s)", err, *timeout)
		}
		fmt.Fprintf(stderr, "mansioctl: %v\n", err)
		return exitError
	}
	if *format == "table" {
		fmt.Fprintln(stdout, v.table)
		if v.note != "" {
			fmt.Fprintf(stdout, "\n%s\n", v.note)
		}
		return exitOK
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v.json); err != nil {
		fmt.Fprintf(stderr, "mansioctl: cannot write the answer: %v\n", err)
		return exitError
	}
	if v.note != "" {
		fmt.Fprintf(stderr, "mansioctl: %s\n", v.note)
	}
	return exitOK
}

// errUsageShown is a usage error whose explanation is already on stderr.
var errUsageShown = errors.New("usage shown")

// parseCommand finds the group and command in args and parses the command's flags and arguments.
func parseCommand(args []string, stderr io.Writer) (action, error) {
	if len(args) == 0 {
		return nil, errors.New("no command group")
	}
	g := groups[args[0]]
	if g == nil {
		return nil, fmt.Errorf("unknown command group %q (groups: %s)", args[0], strings.Join(sortedKeys(groups), ", "))
	}
	if len(args) == 1 {
		return nil, fmt.Errorf("%s: no command (commands: %s)", args[0], strings.Join(sortedKeys(g.commands), ", "))
	}
	cmd := g.commands[args[1]]
	if cmd == nil {
		return nil, fmt.Errorf("%s: unknown command %q (commands: %s)", args[0], args[1], strings.Join(sortedKeys(g.commands), ", "))
	}
	name := "mansioctl " + args[0] + " " + args[1]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s %s\n  %s\n", name, cmd.usage, cmd.summary)
		fs.PrintDefaults()
	}
	check := cmd.parse(fs)
	pos, err := parseInterleaved(fs, args[2:])
	if err != nil {
		return nil, errUsageShown // the flag package explained it
	}
	run, err := check(pos)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (usage: %s %s)", args[0]+" "+args[1], err, name, cmd.usage)
	}
	return run, nil
}

// parseInterleaved parses flags that come before, between or after the positional arguments (so both
// "create -slug s Acme" and "create Acme -slug s" work) and returns the positional arguments. After "--"
// everything is positional.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			return append(pos, rest...), nil
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

func printUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, usageHead+"\n")
	for _, gn := range sortedKeys(groups) {
		g := groups[gn]
		fmt.Fprintf(w, "%s: %s\n", gn, g.summary)
		for _, cn := range sortedKeys(g.commands) {
			c := g.commands[cn]
			fmt.Fprintf(w, "  %s %s %s\n      %s\n", gn, cn, c.usage, c.summary)
		}
	}
	fmt.Fprintln(w, "\nglobal flags (before the group):")
	fs.PrintDefaults()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// checkAddr parses -addr: an http or https URL with a host and no credentials in it.
func checkAddr(addr string) (*url.URL, error) {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("-addr %q: want an http(s) URL with a host and no credentials, query or fragment", addr)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// maxTokenFile bounds the token file read.
const maxTokenFile = 64 << 10

// adminToken reads the admin token from file (the token alone, or a JSON object with admin_token), else from
// $ADMIN_TOKEN. A file must not be readable by group or others, as the daemons' secrets files.
func adminToken(file string, getenv func(string) string) (string, error) {
	token := getenv("ADMIN_TOKEN")
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("cannot read -token-file: %w", err)
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return "", fmt.Errorf("cannot read -token-file: %w", err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf("-token-file %s has mode %#o; make it 0600 (chmod 600)", file, perm)
		}
		b, err := io.ReadAll(io.LimitReader(f, maxTokenFile))
		if err != nil {
			return "", fmt.Errorf("cannot read -token-file: %w", err)
		}
		token = strings.TrimSpace(string(b))
		if strings.HasPrefix(token, "{") {
			var sec struct {
				AdminToken string `json:"admin_token"`
			}
			if json.Unmarshal(b, &sec) != nil || sec.AdminToken == "" {
				return "", fmt.Errorf("-token-file %s is a JSON object without a string admin_token", file)
			}
			token = sec.AdminToken
		}
	}
	if token == "" {
		return "", errors.New("no admin token: set -token-file FILE or ADMIN_TOKEN")
	}
	if err := checkText("the admin token", token); err != nil {
		return "", err
	}
	return token, nil
}

// checkText refuses empty or blank text and text with control characters (C0 or DEL): they would corrupt a
// terminal or a header.
func checkText(label, v string) error {
	if strings.TrimSpace(v) == "" || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%s must be nonempty text without control characters", label)
	}
	return nil
}
