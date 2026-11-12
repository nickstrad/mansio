package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"net/url"
	"time"
)

// The controld groups: databases, vms, keys and limits. Each command is a thin wrapper over one endpoint of
// controld's API (cmd/controld/api.go), so every admin action is scriptable: flags only, the request built from
// them, the answer printed as controld sent it, exit 1 on any status but 2xx.

// defaultControldTimeout is -timeout's default for the controld groups: a move or a restore waits for agents.
const defaultControldTimeout = 3 * time.Minute

var databasesGroup = &group{
	summary: "database lifecycle (controld's /v1/databases)",
	target:  apiTarget, addr: controldAddr, timeout: defaultControldTimeout,
	commands: map[string]*command{
		"create": apiCommand("-tenant T -name N", "create a database (placed by the policy)", func(fs *flag.FlagSet) request {
			tenant, name := fs.String("tenant", "", "tenant id"), fs.String("name", "", "database name")
			return func() (string, string, any, error) {
				return http.MethodPost, "/v1/databases", map[string]string{"tenant": *tenant, "name": *name}, need(*tenant, *name)
			}
		}),
		"get": apiCommand("-db KEY", "a database's registry state (active, moving, suspended, dropped)", dbRequest(http.MethodGet, "")),
		"move": apiCommand("-db KEY [-to VM]", "move a database to another vm", func(fs *flag.FlagSet) request {
			db, to := fs.String("db", "", "database key (tenant:name)"), fs.String("to", "", "destination vm (default: the policy picks)")
			return func() (string, string, any, error) {
				return http.MethodPost, "/v1/databases/" + url.PathEscape(*db) + "/move", map[string]string{"to": *to}, need(*db)
			}
		}),
		"suspend": apiCommand("-db KEY", "suspend a database (written back, served nowhere)", dbRequest(http.MethodPost, "/suspend")),
		"resume": apiCommand("-db KEY [-to VM]", "resume a suspended database", func(fs *flag.FlagSet) request {
			db, to := fs.String("db", "", "database key (tenant:name)"), fs.String("to", "", "destination vm (default: the policy picks)")
			return func() (string, string, any, error) {
				return http.MethodPost, "/v1/databases/" + url.PathEscape(*db) + "/resume", map[string]string{"to": *to}, need(*db)
			}
		}),
		"drop": apiCommand("-db KEY", "drop a database and delete its objects (cannot be undone)", dbRequest(http.MethodPost, "/drop")),
	},
}

var vmsGroup = &group{
	summary: "the VM registry and restoring a dead VM's databases (controld's /v1/vms)",
	target:  apiTarget, addr: controldAddr, timeout: defaultControldTimeout,
	commands: map[string]*command{
		"list": apiCommand("", "the registered vms and their heartbeats", func(*flag.FlagSet) request {
			return func() (string, string, any, error) { return http.MethodGet, "/v1/vms", nil, nil }
		}),
		"attest-dead": apiCommand("-vm VM -by WHO", "record that an operator confirmed a vm dead (accepts the loss of its unpublished writes)",
			func(fs *flag.FlagSet) request {
				vm, by := fs.String("vm", "", "the vm an operator confirmed dead"), fs.String("by", "", "who attests it")
				return func() (string, string, any, error) {
					return http.MethodPost, "/v1/vms/" + url.PathEscape(*vm) + "/attest-dead", map[string]string{"by": *by}, need(*vm, *by)
				}
			}),
		"restore": apiCommand("-vm VM", "move a vm attested dead's databases to live vms", func(fs *flag.FlagSet) request {
			vm := fs.String("vm", "", "a vm attested dead: move its databases elsewhere")
			return func() (string, string, any, error) {
				return http.MethodPost, "/v1/vms/" + url.PathEscape(*vm) + "/restore", nil, need(*vm)
			}
		}),
	},
}

var keysGroup = &group{
	summary: "control-plane API keys for the locator (controld's /v1/tenants/{t}/api-keys, /v1/api-keys)",
	target:  apiTarget, addr: controldAddr, timeout: defaultControldTimeout,
	commands: map[string]*command{
		"create": apiCommand("-tenant T -principal P [-perms read|write|read|write]", "issue an API key for a principal of a tenant (printed once)",
			func(fs *flag.FlagSet) request {
				tenant, principal := fs.String("tenant", "", "tenant id"), fs.String("principal", "", "who the key is for (the tickets' sub)")
				perms := fs.String("perms", "read|write", "read, write or read|write")
				return func() (string, string, any, error) {
					return http.MethodPost, "/v1/tenants/" + url.PathEscape(*tenant) + "/api-keys", map[string]string{"principal": *principal, "perms": *perms},
						need(*tenant, *principal)
				}
			}),
		"revoke": apiCommand("-id KEY_ID", "revoke an API key", func(fs *flag.FlagSet) request {
			id := fs.String("id", "", "the key's id (the 16 hex digits after mansio_)")
			return func() (string, string, any, error) {
				return http.MethodPost, "/v1/api-keys/" + url.PathEscape(*id) + "/revoke", nil, need(*id)
			}
		}),
	},
}

var limitsGroup = &group{
	summary: "controld's slot pools (controld's /v1/limits)",
	target:  apiTarget, addr: controldAddr, timeout: defaultControldTimeout,
	commands: map[string]*command{
		"set": apiCommand("-queue QUEUE -n N", "a slot pool's limit from now on (not written to the config)", func(fs *flag.FlagSet) request {
			queue := fs.String("queue", "", "the slot pool: resolve_load, resolve_create, controld_api, tenancy_admin or tenancy_auth")
			n := fs.Int("n", 0, "its limit from now on (>= 1)")
			return func() (string, string, any, error) {
				if *n < 1 {
					return "", "", nil, errors.New("-n must be >= 1")
				}
				return http.MethodPost, "/v1/limits/" + url.PathEscape(*queue), map[string]int{"limit": *n}, need(*queue)
			}
		}),
	},
}

// request builds a controld call from the parsed flags: method, path, body (nil: none), or a usage error.
type request func() (method, path string, body any, err error)

// apiCommand is a controld call: build registers its flags and returns the request; positional arguments are
// a usage error.
func apiCommand(usage, summary string, build func(fs *flag.FlagSet) request) *command {
	return &command{usage, summary, func(fs *flag.FlagSet) func([]string) (action, error) {
		req := build(fs)
		return func(args []string) (action, error) {
			if len(args) > 0 {
				return nil, errors.New("takes flags only, got the argument " + args[0])
			}
			method, path, body, err := req()
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, c *client) (view, error) { return c.raw(ctx, method, path, body) }, nil
		}
	}}
}

func dbRequest(method, suffix string) func(fs *flag.FlagSet) request {
	return func(fs *flag.FlagSet) request {
		db := fs.String("db", "", "database key (tenant:name)")
		return func() (string, string, any, error) {
			return method, "/v1/databases/" + url.PathEscape(*db) + suffix, nil, need(*db)
		}
	}
}

func need(values ...string) error {
	for _, v := range values {
		if v == "" {
			return errors.New("missing a required flag")
		}
	}
	return nil
}
