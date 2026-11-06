package pg

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// The Deno platform's tenancy import (replace-platform plan §4.3; IR-4a): its Postgres tables (migrations 001
// tenants/api_keys and 002 Better Auth user/account/session/memberships) into this schema's (0007). Legacy keys
// keep working (their whole-key hash is kept), passwords verify unchanged (tenancy's Better Auth scrypt), and
// unexpired sessions move by SHA-256 of their token. Rows already present are skipped, so a re-run is safe.

// MansioTenancy is what ReadMansio read: one JSON document (import_mansio_core.sql merged with
// import_mansio_auth.sql). It holds hashes, never a key, token or password.
type MansioTenancy struct {
	Doc     map[string]json.RawMessage
	HasAuth bool // the source had Better Auth's tables
}

// ImportCounts is how many rows each table gained (or would gain, in a dry run).
type ImportCounts struct {
	Tenants, APIKeys, Users, Passwords, Sessions, Memberships int64
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
	if err := conn.QueryRow(ctx, q.importHasAuth).Scan(&out.HasAuth); err != nil {
		return out, errs.Wrap(module, err, "cannot import: cannot read the source; nothing changed")
	}
	var core []byte
	if err := conn.QueryRow(ctx, q.importCore).Scan(&core); err != nil {
		return out, errs.Wrap(module, err, "cannot import: cannot read the source tenants and api_keys (is this the platform's database?); nothing changed")
	}
	if err := json.Unmarshal(core, &out.Doc); err != nil {
		return out, errs.Wrap(module, err, "cannot import: the source answered no JSON object (a bug)")
	}
	if out.HasAuth {
		var auth []byte
		if err := conn.QueryRow(ctx, q.importAuth).Scan(&auth); err != nil {
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
	doc, err := json.Marshal(src.Doc)
	if err != nil {
		return c, errs.Wrap(module, err, "cannot import: cannot encode the source document (a bug)")
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
	err = tx.QueryRow(ctx, q.importMansio, doc).Scan(&c.Tenants, &c.APIKeys, &c.Users, &c.Passwords, &c.Sessions, &c.Memberships)
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
