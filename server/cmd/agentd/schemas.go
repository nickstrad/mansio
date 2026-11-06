package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// validSchema is the registry client's Validate hook: a fetched text must be a valid schema in fingerprint
// form (docexec.CanonicalText), or the agent refuses to create databases from it.
func validSchema(_ string, text []byte) error { return docexec.CanonicalText(string(text)) }

// registrySchemas is the front's SchemaSource over controld's registry (durable mode, schemas_url).
type registrySchemas struct{ c *schemareg.Client }

func (r registrySchemas) Schema(ctx context.Context, app, fingerprint string) (string, error) {
	text, err := r.c.Get(ctx, app, fingerprint)
	switch {
	case err == nil:
		return text, nil
	case errors.Is(err, schemareg.ErrNotFound), errors.Is(err, schemareg.ErrInvalid):
		return "", fmt.Errorf("%w: %w", front.ErrSchemaUnknown, err)
	case errors.Is(err, schemareg.ErrUnavailable):
		return "", fmt.Errorf("%w: %w", front.ErrUnavailable, err)
	}
	return "", err // the caller's ctx ended
}

// dirSchemas is static mode's registry (schemas_dir): every schema read at start, by app and fingerprint.
type dirSchemas map[[2]string]string

func (d dirSchemas) Schema(_ context.Context, app, fingerprint string) (string, error) {
	if text, ok := d[[2]string{app, fingerprint}]; ok {
		return text, nil
	}
	return "", fmt.Errorf("%w: %s has no %s in schemas_dir", front.ErrSchemaUnknown, app, fingerprint)
}

// loadSchemaDir reads schemas_dir: <app>/<name>.json, each a schema in fingerprint form (a trailing newline is
// not part of it). "" = none (nil). Every file must be valid: a typo fails startup rather than the first
// request.
func loadSchemaDir(dir string) (front.SchemaSource, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.json"))
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot list schemas_dir").With("dir", dir)
	}
	out := dirSchemas{}
	for _, f := range files {
		app := filepath.Base(filepath.Dir(f))
		if !pg.ValidName(app) {
			return nil, errs.Wrap(module, nil, "schemas_dir: %q is not an app name ([a-z0-9][a-z0-9_-]{0,55})", app).With("path", f)
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, errs.Wrap(module, err, "cannot read a schema in schemas_dir").With("path", f)
		}
		text := bytes.TrimRight(raw, "\r\n")
		if err := docexec.CanonicalText(string(text)); err != nil {
			return nil, errs.Wrap(module, err, "schemas_dir: not a schema in fingerprint form (JSON.stringify of the schema)").With("path", f)
		}
		out[[2]string{app, schemareg.Fingerprint(text)}] = string(text)
	}
	return out, nil
}
