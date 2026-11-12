package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The tenants group: tenants and their API keys over the admin API's /admin/tenants routes
// (internal/tenancy/tenancyhttp). A fresh key's plaintext is printed by create and issue-key only; list and
// show print metadata, never a key.

var tenantsGroup = &group{
	summary: "tenants and their API keys (the admin API's /admin/tenants, on agentd or controld)",
	target:  apiTarget, addr: agentdAddr, timeout: 30 * time.Second, tables: true,
	commands: map[string]*command{
		"create": {"<name> [-slug S]", "create an active tenant and its first API key, \"default\" (the key is printed once)",
			func(fs *flag.FlagSet) func([]string) (action, error) {
				slug := fs.String("slug", "", "the tenant's slug (default: the name in lowercase, each run of other characters a -)")
				return func(args []string) (action, error) {
					name, err := oneArg(args, "name")
					if err != nil {
						return nil, err
					}
					s := *slug
					if s == "" {
						s = deriveSlug(name)
						if s == "" {
							return nil, errors.New("the name has no ASCII letters or digits to make a slug from; give -slug")
						}
					}
					if err := checkOption("-slug", s); err != nil {
						return nil, err
					}
					return func(ctx context.Context, c *client) (view, error) {
						var out struct {
							Tenant tenantJSON `json:"tenant"`
							APIKey issuedJSON `json:"apiKey"`
						}
						if err := c.call(ctx, http.MethodPost, "/admin/tenants", map[string]string{"name": name, "slug": s}, &out); err != nil {
							return view{}, err
						}
						if err := out.APIKey.check(); err != nil {
							return view{}, err
						}
						return view{json: out, note: keyNote,
							table: tenantTable(out.Tenant) + "\n\n" + out.APIKey.table()}, nil
					}, nil
				}
			}},
		"list": {"", "list the tenants, oldest first",
			func(fs *flag.FlagSet) func([]string) (action, error) {
				return func(args []string) (action, error) {
					if len(args) != 0 {
						return nil, fmt.Errorf("takes no arguments, got %d", len(args))
					}
					return func(ctx context.Context, c *client) (view, error) {
						out := []tenantJSON{}
						if err := c.call(ctx, http.MethodGet, "/admin/tenants", nil, &out); err != nil {
							return view{}, err
						}
						return view{json: out, table: tenantTable(out...)}, nil
					}, nil
				}
			}},
		"show":       {"<id>", "show one tenant", tenantCall(http.MethodGet, "")},
		"deactivate": {"<id>", "deactivate a tenant and revoke its keys (idempotent)", tenantCall(http.MethodDelete, "")},
		"issue-key": {"<id> [-name N]", "issue a new API key for an active tenant (the key is printed once)",
			func(fs *flag.FlagSet) func([]string) (action, error) {
				name := fs.String("name", "default", "the key's name")
				return func(args []string) (action, error) {
					id, err := oneArg(args, "id")
					if err != nil {
						return nil, err
					}
					if err := checkTenantID(id); err != nil {
						return nil, err
					}
					if err := checkOption("-name", *name); err != nil {
						return nil, err
					}
					return func(ctx context.Context, c *client) (view, error) {
						var out issuedJSON
						if err := c.call(ctx, http.MethodPost, "/admin/tenants/"+url.PathEscape(id)+"/keys", map[string]string{"name": *name}, &out); err != nil {
							return view{}, err
						}
						if err := out.check(); err != nil {
							return view{}, err
						}
						return view{json: out, table: out.table(), note: keyNote}, nil
					}, nil
				}
			}},
		"revoke-key": {"<key-id>", "revoke an API key by its id (16 hex digits, or a lowercase uuid for an imported key)",
			func(fs *flag.FlagSet) func([]string) (action, error) {
				return func(args []string) (action, error) {
					id, err := oneArg(args, "key id")
					if err != nil {
						return nil, err
					}
					if !keyIDRE.MatchString(id) {
						return nil, errors.New("key id must be 16 hex digits or a lowercase uuid")
					}
					return func(ctx context.Context, c *client) (view, error) { return revokeKey(ctx, c, id) }, nil
				}
			}},
	},
}

// keyNote follows a fresh key.
const keyNote = "Save this API key now; it will not be shown again."

// revokeKey revokes key id. The admin API revokes a key only under its tenant's URL and has no lookup from a key
// to its tenant, so this asks every tenant in turn: revocation is idempotent and leaves another tenant's key
// alone, so trying them all is safe and keeps the command to one argument.
func revokeKey(ctx context.Context, c *client, id string) (view, error) {
	var tenants []tenantJSON
	if err := c.call(ctx, http.MethodGet, "/admin/tenants", nil, &tenants); err != nil {
		return view{}, err
	}
	for _, t := range tenants {
		if !uuidRE.MatchString(t.ID) {
			return view{}, fmt.Errorf("the tenant list holds an invalid tenant id %q", sanitize(t.ID))
		}
		var out struct{}
		if err := c.call(ctx, http.MethodDelete, "/admin/tenants/"+t.ID+"/keys/"+id, nil, &out); err != nil {
			return view{}, err
		}
	}
	res := struct {
		ID      string `json:"id"`
		Revoked bool   `json:"revoked"`
	}{id, true}
	return view{json: res, table: table([]string{"id", "revoked"}, [][]string{{id, "true"}})}, nil
}

// tenantCall is show and deactivate: method on /admin/tenants/{id}, answering one tenant.
func tenantCall(method, suffix string) func(fs *flag.FlagSet) func([]string) (action, error) {
	return func(fs *flag.FlagSet) func([]string) (action, error) {
		return func(args []string) (action, error) {
			id, err := oneArg(args, "id")
			if err != nil {
				return nil, err
			}
			if err := checkTenantID(id); err != nil {
				return nil, err
			}
			return func(ctx context.Context, c *client) (view, error) {
				var out tenantJSON
				if err := c.call(ctx, method, "/admin/tenants/"+url.PathEscape(id)+suffix, nil, &out); err != nil {
					return view{}, err
				}
				return view{json: out, table: tenantTable(out)}, nil
			}, nil
		}
	}
}

// tenantJSON is a tenant as the admin API answers it. Decoding into it keeps only these fields.
type tenantJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

// issuedJSON is a fresh API key: its metadata and, once, its plaintext.
type issuedJSON struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenantId"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"createdAt"`
	Plaintext string `json:"plaintext"`
}

var plaintextRE = regexp.MustCompile(`^mansio_[A-Za-z0-9_-]+$`)

func (k issuedJSON) check() error {
	if !plaintextRE.MatchString(k.Plaintext) {
		return errors.New("the answer holds no issued API key")
	}
	return nil
}

func (k issuedJSON) table() string {
	return table([]string{"id", "tenantId", "name", "createdAt"}, [][]string{{k.ID, k.TenantID, k.Name, k.CreatedAt}}) +
		"\n\nAPI key: " + k.Plaintext
}

// tenantTable renders tenants with explicit columns, so nothing but this metadata can reach the table.
func tenantTable(ts ...tenantJSON) string {
	rows := make([][]string, len(ts))
	for i, t := range ts {
		rows[i] = []string{t.ID, t.Name, t.Slug, t.Status, t.CreatedAt}
	}
	return table([]string{"id", "name", "slug", "status", "createdAt"}, rows)
}

// table renders rows under columns, padded to the widest cell, with a row count. Control characters in cells
// become spaces so server text stays on one terminal line.
func table(columns []string, rows [][]string) string {
	widths := make([]int, len(columns))
	for i, c := range columns {
		widths[i] = len(c)
	}
	clean := make([][]string, len(rows))
	for r, row := range rows {
		clean[r] = make([]string, len(columns))
		for i := range columns {
			clean[r][i] = sanitize(row[i])
			widths[i] = max(widths[i], len([]rune(clean[r][i])))
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		var l strings.Builder
		for i, c := range cells {
			if i > 0 {
				l.WriteString("  ")
			}
			l.WriteString(c + strings.Repeat(" ", widths[i]-len([]rune(c))))
		}
		b.WriteString(strings.TrimRight(l.String(), " ") + "\n")
	}
	line(columns)
	dashes := make([]string, len(columns))
	for i, w := range widths {
		dashes[i] = strings.Repeat("-", w)
	}
	line(dashes)
	for _, row := range clean {
		line(row)
	}
	plural := "s"
	if len(rows) == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "(%d row%s)", len(rows), plural)
	return b.String()
}

// sanitize replaces control characters (C0, DEL, C1) with spaces.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
}

var (
	uuidRE  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	keyIDRE = regexp.MustCompile(`^(?:[0-9a-f]{16}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
	slugRE  = regexp.MustCompile(`[^a-z0-9]+`)
)

// deriveSlug is the default slug: the name in lowercase, each run of characters other than a-z and 0-9 one -,
// none at either end. "" if nothing is left.
func deriveSlug(name string) string {
	return strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

func checkTenantID(id string) error {
	if !uuidRE.MatchString(id) {
		return errors.New("id must be a canonical lowercase uuid")
	}
	return nil
}

// oneArg checks that args is exactly one valid text argument.
func oneArg(args []string, label string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("want exactly one %s, got %d arguments", label, len(args))
	}
	return args[0], checkText(label, args[0])
}

// checkOption checks a flag's text value; a value starting with -- is most likely a forgotten value.
func checkOption(flag, v string) error {
	if strings.HasPrefix(v, "--") {
		return fmt.Errorf("%s needs a value", flag)
	}
	return checkText(flag, v)
}
