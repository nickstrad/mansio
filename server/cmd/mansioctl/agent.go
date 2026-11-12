package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// The agent group: one agentd's runtime controls on its admin listener (cmd/agentd/admin.go, admin_listen,
// loopback only). Each command POSTs one path with no body; agentd answers {"code": "ok", ...} or an error
// code and message.

var agentGroup = &group{
	summary: "one agentd's runtime controls (its loopback admin listener; -agent HOST:PORT)",
	target:  agentTarget,
	timeout: defaultAgentTimeout,
	commands: map[string]*command{
		"breaker":     agentCommand("breaker", "<dep> trip|reset", "force a breaker open until reset, or close it (dep: s3, placement)"),
		"fetch-limit": agentCommand("fetch-limit", "<n>", "S3 fetches in flight from now on (n >= 1)"),
		"limit": agentCommand("limit", "<queue> <n>",
			"a slot pool's limit from now on (queue: verify, lifecycle, credential_place, tenancy_admin, tenancy_auth; n >= 1)"),
		"drain":     agentCommand("drain", "", "stop taking queries (one way; /readyz says draining)"),
		"passivate": agentCommand("passivate", "<key>", "write one warm database back and drop its local copy"),
		"backup":    agentCommand("backup", "<key>", "write one database back now and print its manifest (it stays warm)"),
		"loglevel":  agentCommand("loglevel", "debug|info|warn", "change the log level"),
	},
}

// defaultAgentTimeout is -timeout's default for the agent group.
const defaultAgentTimeout = 10 * time.Second

// agentCommand is one agent control: its arguments become the admin listener's path (agentPath).
func agentCommand(name, usage, summary string) *command {
	return &command{usage, summary, func(*flag.FlagSet) func([]string) (action, error) {
		return func(args []string) (action, error) {
			path, err := agentPath(append([]string{name}, args...))
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, c *client) (view, error) { return c.agent(ctx, path) }, nil
		}
	}}
}

// agentPath validates a control's arguments (args[0] is the control) and returns its path and query.
func agentPath(args []string) (string, error) {
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
	case "limit":
		if err := want(2); err != nil {
			return "", err
		}
		if n, err := strconv.Atoi(args[2]); err != nil || n < 1 {
			return "", fmt.Errorf("limit %s %q: want an integer >= 1", args[1], args[2])
		}
		return "/admin/limit?queue=" + url.QueryEscape(args[1]) + "&n=" + args[2], nil
	case "drain":
		return "/admin/drain", want(0)
	case "passivate":
		if err := want(1); err != nil {
			return "", err
		}
		return "/admin/passivate?key=" + url.QueryEscape(args[1]), nil
	case "backup":
		if err := want(1); err != nil {
			return "", err
		}
		return "/admin/backup?key=" + url.QueryEscape(args[1]), nil
	case "loglevel":
		if err := want(1); err != nil {
			return "", err
		}
		return "/admin/loglevel?level=" + url.QueryEscape(args[1]), nil
	}
	return "", errors.New("unknown agent control " + strconv.Quote(args[0]))
}
