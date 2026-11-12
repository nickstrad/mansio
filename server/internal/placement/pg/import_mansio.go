package pg

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// The Deno platform's tenancy import: its Postgres tables (migrations 001 tenants/api_keys and 002 Better Auth
// user/account/session/memberships) into this schema's (0007). Legacy keys keep working (their whole-key hash is
// kept), passwords verify unchanged (tenancy's Better Auth scrypt), and unexpired sessions move by SHA-256 of their
// token. Rows already present are skipped, so a re-run is safe.

// MansioTenancy is what ReadMansio read: one JSON document (import_mansio_core.sql merged with
// import_mansio_auth.sql). It holds hashes, never a key, token or password.
type MansioTenancy struct {
	Doc     map[string]json.RawMessage
	HasAuth bool // the source had Better Auth's tables
}

// ImportCounts is how many rows each table gained (or would gain, in a dry run), and the one-way changes a
// re-run carried (import_mansio.sql).
type ImportCounts struct {
	Tenants, TenantsDeactivated, APIKeys, KeysRevoked, Users, Passwords, PasswordsChanged, Sessions, Memberships int64
}

// importTimeout bounds each side of an import (one read per source query, the one write).
const importTimeout = 60 * time.Second

// ReadMansio reads the Deno platform's tenancy from the database at dsn, tables in schema ("" = the server's
// search_path). It changes nothing there.
func ReadMansio(ctx context.Context, dsn, schema string) (MansioTenancy, error) {
	var out MansioTenancy
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return out, errs.Wrap(module, nil, "cannot import: the source dsn does not parse") // the error may quote the password
	}
	if schema != "" {
		if !schemaName.MatchString(schema) {
			return out, errs.Wrap(module, nil, "cannot import: source schema %q must match %s", schema, schemaName)
		}
		cfg.RuntimeParams["search_path"] = schema
	}
	cfg.RuntimeParams["application_name"] = "controld/import-mansio"
	cfg.RuntimeParams["default_transaction_read_only"] = "on" // the source is only read
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return out, errs.Wrap(module, marked{err, ErrUnavailable}, "cannot import: cannot connect to the source; nothing changed")
	}
	defer conn.Close(context.Background())
	// One snapshot for every read: keys never refer to a tenant the tenants read did not see.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, errs.Wrap(module, marked{err, ErrUnavailable}, "cannot import: cannot start a read on the source; nothing changed")
	}
	defer tx.Rollback(context.Background())
	if err := tx.QueryRow(ctx, q.importHasAuth).Scan(&out.HasAuth); err != nil {
		return out, errs.Wrap(module, err, "cannot import: cannot read the source; nothing changed")
	}
	var core []byte
	if err := tx.QueryRow(ctx, q.importCore).Scan(&core); err != nil {
		return out, errs.Wrap(module, err, "cannot import: cannot read the source tenants and api_keys (is this the platform's database?); nothing changed")
	}
	if err := json.Unmarshal(core, &out.Doc); err != nil {
		return out, errs.Wrap(module, err, "cannot import: the source answered no JSON object (a bug)")
	}
	if out.HasAuth {
		var auth []byte
		if err := tx.QueryRow(ctx, q.importAuth).Scan(&auth); err != nil {
			return out, errs.Wrap(module, err, "cannot import: cannot read the source auth tables; nothing changed")
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(auth, &doc); err != nil {
			return out, errs.Wrap(module, err, "cannot import: the source answered no JSON object (a bug)")
		}
		for k, v := range doc {
			out.Doc[k] = v
		}
	}
	return out, nil
}

// ImportMansio writes src into p's tables in one transaction (one statement). With dryRun it rolls back:
// the counts say what a real run would add, and nothing changes.
func (p *Placement) ImportMansio(ctx context.Context, src MansioTenancy, dryRun bool) (ImportCounts, error) {
	var c ImportCounts
	doc, users, err := fitMansio(src.Doc)
	if err != nil {
		return c, err
	}
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return c, errs.Wrap(module, marked{err, ErrUnavailable}, "cannot import: no connection to the target; nothing changed, safe to retry")
	}
	defer tx.Rollback(context.Background()) // after Commit: a no-op
	// The one statement may be long for a large source: this transaction's own bound, not the pool's 2 s.
	if _, err := tx.Exec(ctx, "set local statement_timeout = '50s'"); err != nil {
		return c, errs.Wrap(module, classify(err), "cannot import: nothing changed, safe to retry")
	}
	rows, err := tx.Query(ctx, q.importEmailTaken, users)
	if err != nil {
		return c, errs.Wrap(module, classify(err), "cannot import: nothing changed, safe to retry")
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return c, errs.Wrap(module, classify(err), "cannot import: nothing changed, safe to retry")
	}
	if len(taken) > 0 {
		return c, errs.Wrap(module, nil, "cannot import: another user here already has the email of source user(s) %s; nothing changed",
			strings.Join(taken, ", "))
	}
	err = tx.QueryRow(ctx, q.importMansio, doc).Scan(&c.Tenants, &c.TenantsDeactivated, &c.APIKeys, &c.KeysRevoked, &c.Users,
		&c.Passwords, &c.PasswordsChanged, &c.Sessions, &c.Memberships)
	if err != nil {
		cls := classify(err)
		if errors.Is(cls, ErrInvariant) || errors.Is(cls, ErrUnknownVM) { // a constraint: the source has a row the target refuses
			return c, errs.Wrap(module, cls, "cannot import: a source row breaks a target constraint (see the cause); nothing changed")
		}
		return c, errs.Wrap(module, cls, "cannot import: nothing changed, safe to retry")
	}
	if dryRun {
		return c, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return c, errs.Wrap(module, marked{err, ErrUnavailable}, "cannot import: the commit got no answer; re-run (rows already imported are skipped)")
	}
	return c, nil
}

// maxField is the target's text bound for slugs and names (0007: octet_length <= 200); emails 254.
const maxField, maxEmail = 200, 254

// fitMansio checks the source against the target's bounds, which the Deno platform did not have: a slug or
// email too long is an error naming its row (it is an identity: it cannot be cut); a name too long is cut at a
// character boundary to fit. It returns the document to write and the users' {id, email} for the email check.
func fitMansio(doc map[string]json.RawMessage) (out, users []byte, err error) {
	rows := func(key string) ([]map[string]any, error) {
		var r []map[string]any
		if raw, ok := doc[key]; ok {
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, errs.Wrap(module, err, "cannot import: the source %s are not a JSON array (a bug)", key)
			}
		}
		return r, nil
	}
	tenants, err := rows("tenants")
	if err != nil {
		return nil, nil, err
	}
	for _, t := range tenants {
		if slug, _ := t["slug"].(string); len(slug) > maxField {
			return nil, nil, errs.Wrap(module, nil, "cannot import: tenant %v has a slug of %d bytes (at most %d here); shorten it at the source; nothing changed",
				t["id"], len(slug), maxField)
		}
		cut(t, "name", maxField)
	}
	us, err := rows("users")
	if err != nil {
		return nil, nil, err
	}
	pairs := make([]map[string]any, len(us))
	for i, u := range us {
		if email, _ := u["email"].(string); len(email) > maxEmail {
			return nil, nil, errs.Wrap(module, nil, "cannot import: user %v has an email of %d bytes (at most %d); nothing changed", u["id"], len(email), maxEmail)
		}
		if name, _ := u["name"].(string); name == "" {
			u["name"] = u["email"] // auth_users.name is required; Better Auth allowed ''
		}
		cut(u, "name", maxField)
		pairs[i] = map[string]any{"id": u["id"], "email": u["email"]}
	}
	next := maps.Clone(doc)
	for key, v := range map[string]any{"tenants": tenants, "users": us} {
		if _, ok := doc[key]; !ok {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, nil, errs.Wrap(module, err, "cannot import: cannot encode the source document (a bug)")
		}
		next[key] = b
	}
	if out, err = json.Marshal(next); err == nil {
		users, err = json.Marshal(pairs)
	}
	if err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot import: cannot encode the source document (a bug)")
	}
	return out, users, nil
}

// cut shortens row[field] (a string) to at most n bytes, at a character boundary.
func cut(row map[string]any, field string, n int) {
	s, ok := row[field].(string)
	if !ok || len(s) <= n {
		return
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	row[field] = s[:n]
}
