package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The app schema registry (schema 0006, IR-2): one statement per method under the same breaker and deadlines
// as the placement port. Every method checks its input at the edge (an invalid app or text matches
// owner.ErrInvalid; nothing is sent); the schema re-checks the fingerprint whichever path writes.

// RegisterSchema stores text under fingerprint for app (idempotent) and reports whether this call added the
// app's registration. The text must pass schemareg.Check (it hashes to fingerprint).
func (p *Placement) RegisterSchema(ctx context.Context, app, fingerprint string, text []byte) (created bool, err error) {
	if !ValidName(app) {
		return false, errs.Wrap(module, owner.ErrInvalid, "cannot register a schema for app %q: an app must match %s; nothing sent", app, namePattern)
	}
	if err := schemareg.Check(fingerprint, text); err != nil {
		return false, errs.Wrap(module, errors.Join(owner.ErrInvalid, err), "cannot register a schema for app %q; nothing sent", app)
	}
	err = p.call(ctx, "register schema "+fingerprint+" of app", app, "safe to retry (idempotent)", func(ctx context.Context) error {
		return p.pool.QueryRow(ctx, q.registerSchema, app, fingerprint, string(text)).Scan(&created)
	})
	return created, err
}

// GetSchema returns the text app registered under fingerprint; an unknown fingerprint, or one only another app
// registered, matches owner.ErrNotFound.
func (p *Placement) GetSchema(ctx context.Context, app, fingerprint string) ([]byte, error) {
	if !ValidName(app) || !schemareg.ValidFingerprint(fingerprint) {
		return nil, errs.Wrap(module, owner.ErrInvalid, "cannot read schema %q of app %q: malformed; nothing sent", fingerprint, app)
	}
	var text string
	err := p.call(ctx, "read schema "+fingerprint+" of app", app, retryRead, func(ctx context.Context) error {
		err := p.pool.QueryRow(ctx, q.getSchema, app, fingerprint).Scan(&text)
		if errors.Is(err, pgx.ErrNoRows) {
			return owner.ErrNotFound
		}
		return err
	})
	return []byte(text), err
}

// AppRegistered reports whether app registered any schema.
func (p *Placement) AppRegistered(ctx context.Context, app string) (bool, error) {
	if !ValidName(app) {
		return false, errs.Wrap(module, owner.ErrInvalid, "cannot look up app %q: an app must match %s; nothing sent", app, namePattern)
	}
	var ok bool
	err := p.call(ctx, "look up app", app, retryRead, func(ctx context.Context) error {
		return p.pool.QueryRow(ctx, q.appRegistered, app).Scan(&ok)
	})
	return ok, err
}
