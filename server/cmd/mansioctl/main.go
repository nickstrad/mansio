// Command mansioctl is the platform's admin CLI: one command for every admin call to a running platform.
//
//	mansioctl [global flags] <group> <command> [flags] [args]
//
//	tenants    tenants and their API keys        admin API /admin/tenants (agentd or controld)
//	databases  database lifecycle                controld's API /v1/databases
//	vms        the VM registry, dead-VM restore  controld's API /v1/vms
//	keys       control-plane API keys            controld's API /v1/tenants/{t}/api-keys, /v1/api-keys
//	limits     controld's slot pools             controld's API /v1/limits
//	agent      one agent's runtime controls      agentd's loopback admin listener (admin_listen)
//
// It reaches two kinds of listener, so it has two address flags. -addr is the API of agentd (the tenant admin
// API, in the api-key and session tenancy modes) or controld, over http or https (-ca for a private CA), with
// the admin token; it defaults to $MANSIOCTL_ADDR, else the group's default (tenants: agentd's
// http://127.0.0.1:7070; the controld groups: http://127.0.0.1:7080). -agent is one agentd's admin listener,
// plain HTTP on loopback with no token (binding to loopback is its access control); it defaults to
// $MANSIOCTL_AGENT_ADDR, else 127.0.0.1:7072. A group uses one of them, and giving the other one's flags is a
// usage error, so an agent address can never receive the admin token and an agent command never goes to an API.
//
// The admin token comes from -token-file (or its alias -secrets, for controld's secrets file) or $ADMIN_TOKEN,
// never from a flag: a command line is visible to other users of the machine and kept in shell history.
//
// Commands come in groups. A group is one file (tenants.go, controlplane.go, agent.go) that defines a *group:
// a map of command name to a usage line and a parse function that checks the arguments and returns the
// action. The groups map below is the registry: adding a group is adding its file and one entry there.
//
// Output: tenants prints one JSON document (-format json, the default) or a table (-format table); the other
// groups print the server's JSON answer as it came. Nothing is stored locally; a fresh API key appears only in
// the output of the command that made it. Exit codes: 0 ok, 1 an error answer or no answer (the message on
// stderr), 2 bad usage, 130 interrupted.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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

// The default addresses: agentd's query listener and controld's API in the sample configs, and agentd's admin
// listener.
const (
	agentdAddr   = "http://127.0.0.1:7070"
	controldAddr = "http://127.0.0.1:7080"
	agentAddr    = "127.0.0.1:7072"
)

// groups is the registry of command groups (mansioctl <group> <command>).
var groups = map[string]*group{
	"tenants":   tenantsGroup,
	"databases": databasesGroup,
	"vms":       vmsGroup,
	"keys":      keysGroup,
	"limits":    limitsGroup,
	"agent":     agentGroup,
}

// target is the kind of listener a group calls.
type target int

const (
	apiTarget   target = iota // agentd's or controld's API: -addr, the admin token, -ca
	agentTarget               // one agentd's loopback admin listener: -agent, no token
)

// group is a set of related commands that call one kind of listener.
type group struct {
	summary  string
	target   target
	addr     string        // apiTarget: -addr's default when neither the flag nor $MANSIOCTL_ADDR is set
	timeout  time.Duration // -timeout's default
	tables   bool          // the commands render tables (-format table)
	commands map[string]*command
}

// command is one admin command: its usage after "mansioctl <group> <name>", a one-line summary, and parse,
// which registers the command's flags on fs and returns a function that checks the positional arguments (after
// the flags are parsed) and returns the action. An error from that function is a usage error (exit 2).
type command struct {
	usage, summary string
	parse          func(fs *flag.FlagSet) func(args []string) (action, error)
}

// action makes the command's calls with c and returns what to print. It may return a view with raw set
// together with an error: the error answer, printed before the error.
type action func(ctx context.Context, c *client) (view, error)

// view is a command's answer. raw, if set, is printed to stdout as it is (the server's own JSON). Otherwise
// json is the document for -format json and table the text for -format table; note, if set, is a line for the
// person running the command (e.g. "save this key"): after the table, or on stderr with JSON so stdout stays
// one document.
type view struct {
	raw   []byte
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

The platform's admin CLI. Two kinds of listener:
  -addr URL         agentd's or controld's API (groups tenants, databases, vms, keys, limits), http or
                    https, with the admin token
  -agent HOST:PORT  one agentd's loopback admin listener (group agent), plain HTTP, no token
Each group takes only its own listener's flags.

The admin token comes from -token-file (or -secrets) or the ADMIN_TOKEN environment variable, never from a
flag: a command line is visible to other users of the machine (ps) and stays in shell history.
`

// realMain runs one command and returns the exit code.
func realMain(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mansioctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "the API's base URL, http or https (default $MANSIOCTL_ADDR, else the group's: tenants "+
		agentdAddr+", databases/vms/keys/limits "+controldAddr+")")
	agent := fs.String("agent", "", "agent group: agentd's admin listener host:port (default $MANSIOCTL_AGENT_ADDR, else "+agentAddr+")")
	var tokenFile string
	fs.StringVar(&tokenFile, "token-file", "", "file holding the admin token (mode 0600): the token alone, or a JSON secrets file with admin_token (default: $ADMIN_TOKEN)")
	fs.StringVar(&tokenFile, "secrets", "", "the same as -token-file (controld's secrets file)")
	ca := fs.String("ca", "", "PEM CA bundle that signs the server's certificate (an https -addr with a private CA)")
	timeout := fs.Duration("timeout", 0, "deadline for the whole command (default: tenants 30s, controld groups 3m, agent 10s; the agent also gets it as X-Timeout-Ms)")
	format := fs.String("format", "json", "tenants output: json (one JSON document) or table")
	fs.Usage = func() { printUsage(stderr, fs) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	g, run, err := parseCommand(fs.Args(), stderr)
	if err == nil {
		err = checkFlags(g, set, *timeout, *format)
	}
	if err != nil {
		if !errors.Is(err, errUsageShown) {
			fmt.Fprintf(stderr, "mansioctl: %v\n", err)
			fmt.Fprintln(stderr, "run mansioctl -h for usage")
		}
		return exitUsage
	}
	if !set["timeout"] {
		*timeout = g.timeout
	}
	c := &client{timeout: *timeout}
	switch g.target {
	case agentTarget:
		a := firstOf(*agent, getenv("MANSIOCTL_AGENT_ADDR"), agentAddr)
		if _, _, err := net.SplitHostPort(a); err != nil {
			fmt.Fprintf(stderr, "mansioctl: -agent %q: want host:port (the admin listener is plain HTTP; no scheme)\n", a)
			return exitUsage
		}
		c.hc, c.base, c.host = &http.Client{}, "http://"+a, a
	default:
		base, err := checkAddr(firstOf(*addr, getenv("MANSIOCTL_ADDR"), g.addr))
		if err != nil {
			fmt.Fprintf(stderr, "mansioctl: %v\n", err)
			return exitUsage
		}
		if c.token, err = adminToken(tokenFile, getenv); err != nil {
			fmt.Fprintf(stderr, "mansioctl: %v\n", err)
			return exitUsage
		}
		if c.hc, err = tlsx.HTTPClient(*ca); err != nil {
			fmt.Fprintf(stderr, "mansioctl: cannot load -ca %s: %v\n", *ca, err)
			return exitError
		}
		// A redirect is answered as an error rather than followed: the token goes only to -addr.
		c.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c.base, c.host = base.String(), base.Host
	}

	cctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	v, err := run(cctx, c)
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "mansioctl: interrupted")
		return exitInterrupted
	}
	if v.raw != nil {
		stdout.Write(v.raw)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w (no answer within -timeout %s)", err, *timeout)
		}
		fmt.Fprintf(stderr, "mansioctl: %v\n", err)
		return exitError
	}
	switch {
	case v.raw != nil:
	case *format == "table":
		fmt.Fprintln(stdout, v.table)
		if v.note != "" {
			fmt.Fprintf(stdout, "\n%s\n", v.note)
		}
	default:
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v.json); err != nil {
			fmt.Fprintf(stderr, "mansioctl: cannot write the answer: %v\n", err)
			return exitError
		}
		if v.note != "" {
			fmt.Fprintf(stderr, "mansioctl: %s\n", v.note)
		}
	}
	return exitOK
}

// checkFlags refuses global flags the group does not use: an API group takes -addr, -token-file/-secrets and
// -ca; the agent group takes -agent. A flag meant for the other listener is a mistake, not something to ignore.
func checkFlags(g *group, set map[string]bool, timeout time.Duration, format string) error {
	other := []string{"agent"}
	if g.target == agentTarget {
		other = []string{"addr", "token-file", "secrets", "ca"}
	}
	for _, f := range other {
		if set[f] {
			if g.target == agentTarget {
				return fmt.Errorf("-%s is for the API groups; the agent group takes -agent HOST:PORT", f)
			}
			return errors.New("-agent is for the agent group; this group takes -addr URL")
		}
	}
	switch {
	case set["timeout"] && timeout <= 0:
		return errors.New("-timeout must be > 0")
	case format != "json" && format != "table":
		return fmt.Errorf("-format %q: want json or table", format)
	case format == "table" && !g.tables:
		return errors.New("-format table is for the tenants group; this group prints the server's JSON answer")
	}
	return nil
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// errUsageShown is a usage error whose explanation is already on stderr.
var errUsageShown = errors.New("usage shown")

// parseCommand finds the group and command in args and parses the command's flags and arguments.
func parseCommand(args []string, stderr io.Writer) (*group, action, error) {
	if len(args) == 0 {
		return nil, nil, errors.New("no command group")
	}
	g := groups[args[0]]
	if g == nil {
		return nil, nil, fmt.Errorf("unknown command group %q (groups: %s)", args[0], strings.Join(sortedKeys(groups), ", "))
	}
	if len(args) == 1 {
		return nil, nil, fmt.Errorf("%s: no command (commands: %s)", args[0], strings.Join(sortedKeys(g.commands), ", "))
	}
	cmd := g.commands[args[1]]
	if cmd == nil {
		return nil, nil, fmt.Errorf("%s: unknown command %q (commands: %s)", args[0], args[1], strings.Join(sortedKeys(g.commands), ", "))
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
		return nil, nil, errUsageShown // the flag package explained it
	}
	run, err := check(pos)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w (usage: %s %s)", args[0]+" "+args[1], err, name, cmd.usage)
	}
	return g, run, nil
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
	for _, gn := range []string{"tenants", "databases", "vms", "keys", "limits", "agent"} {
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
		return "", errors.New("no admin token: set -token-file FILE (or -secrets FILE) or ADMIN_TOKEN")
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
