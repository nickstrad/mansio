package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The app schema registry and app databases (IR protocol plan §2.4, §4; ADR 0011):
//
//	PUT  /v1/apps/{app}/schema                    admin   {"fingerprint", "schema"} → {"code","app","fingerprint","created"}
//	GET  /v1/apps/{app}/schemas/{fingerprint}     admin or control token (agents) → {"code","app","fingerprint","schema"}
//	POST /v1/apps/{app}/databases                 admin   {"tenant"} → the database row (explicit create)
//	POST /v1/resolve                              api key {"tenant","app"[,"create"][,"min_epoch"]} (locator.go)
//
// A database is (tenant, app): the registry's database tenant ":" app, so an app is named by the database-name
// domain (pg.ValidName) and every existing path (move, drop, the agents, durable.KeyPrefix) works on it
// unchanged; resolve's {"tenant": T, "app": X} and raw {"tenant": T, "db": X} name the same database T:X. "schema" is the fingerprint text (ir/spec/v1.md §2): either the JSON object itself, whose bytes are
// taken exactly as sent (a TS client's JSON.stringify({fingerprint, schema}) embeds JSON.stringify(schema)), or a
// JSON string holding that text. Either way the fingerprint must be SHA-256 of those exact bytes.
// Registration is idempotent: created is false when the app already registered that fingerprint.

// schemaStore is the registry's storage (*pg.Placement). Errors as the pg adapter's: owner.ErrInvalid,
// owner.ErrNotFound, pg.ErrUnavailable.
type schemaStore interface {
	RegisterSchema(ctx context.Context, app, fingerprint string, text []byte) (bool, error)
	GetSchema(ctx context.Context, app, fingerprint string) ([]byte, error)
	AppRegistered(ctx context.Context, app string) (bool, error)
}

var _ schemaStore = (*pg.Placement)(nil)

// maxSchemaBody bounds PUT /v1/apps/{app}/schema: the text as a JSON string (≤ 6 bytes per byte) plus the
// envelope.
const maxSchemaBody = 6*schemareg.MaxText + 4<<10

// schemaAnswer is the registry's success body.
type schemaAnswer struct {
	Code        string `json:"code"`
	App         string `json:"app"`
	Fingerprint string `json:"fingerprint"`
	Created     *bool  `json:"created,omitempty"` // PUT only
	Schema      string `json:"schema,omitempty"`  // GET only: the text as a JSON string, so it arrives byte for byte
}

func (d *daemon) schemaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/apps/{app}/schema", d.handle("register-schema", func(ctx context.Context, r *http.Request) (any, error) {
		app := r.PathValue("app")
		if !pg.ValidName(app) {
			return nil, errBadRequest("app must be a database name (" + pg.NamePattern + "): (tenant, app) is the database tenant:app")
		}
		var req struct {
			Fingerprint string          `json:"fingerprint"`
			Schema      json.RawMessage `json:"schema"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxSchemaBody))
		dec.DisallowUnknownFields()
		var tooBig *http.MaxBytesError
		err := dec.Decode(&req)
		if err == nil {
			if err = dec.Decode(&json.RawMessage{}); err == io.EOF {
				err = nil
			} else if !errors.As(err, &tooBig) {
				err = errors.New("trailing data after the object")
			}
		}
		switch {
		case errors.As(err, &tooBig):
			return nil, &apiError{"too_large", http.StatusRequestEntityTooLarge, fmt.Sprintf("body over %d bytes", maxSchemaBody), false}
		case err != nil:
			return nil, errBadRequest("body must be one JSON object {\"fingerprint\", \"schema\"}: " + err.Error())
		}
		text := []byte(req.Schema)
		if len(text) > 0 && text[0] == '"' { // the text as a JSON string
			var s string
			if err := json.Unmarshal(text, &s); err != nil {
				return nil, errBadRequest("schema: " + err.Error())
			}
			text = []byte(s)
		}
		if err := schemareg.Check(req.Fingerprint, text); err != nil {
			return nil, errBadRequest(err.Error())
		}
		if d.checkSchema != nil {
			if err := d.checkSchema(req.Fingerprint, text); err != nil {
				return nil, errBadRequest("not a valid schema: " + err.Error())
			}
		}
		created, err := d.schemas.RegisterSchema(ctx, app, req.Fingerprint, text)
		if err != nil {
			return nil, err
		}
		return schemaAnswer{Code: "ok", App: app, Fingerprint: req.Fingerprint, Created: &created}, nil
	}))
	// The agents' read is on the data path (a database's first request per schema): the locator's bulkhead and
	// deadline, not the admin API's.
	mux.HandleFunc("GET /v1/apps/{app}/schemas/{fingerprint}", d.handleIn(d.loadSem, &d.schemaShed, d.cfg.Locator.Timeout.Duration,
		[]string{d.token, d.control}, "admin or control", "",
		func(ctx context.Context, r *http.Request) (any, error) {
			app, fp := r.PathValue("app"), r.PathValue("fingerprint")
			if !pg.ValidName(app) || !schemareg.ValidFingerprint(fp) {
				return nil, errBadRequest("app must be a database name and fingerprint sha256: + 64 lowercase hex digits")
			}
			text, err := d.schemas.GetSchema(ctx, app, fp)
			if errors.Is(err, owner.ErrNotFound) {
				return nil, &apiError{"not_found", http.StatusNotFound, "app " + app + " registered no schema " + fp, false}
			}
			if err != nil {
				return nil, err
			}
			return schemaAnswer{Code: "ok", App: app, Fingerprint: fp, Schema: string(text)}, nil
		}))
	mux.HandleFunc("POST /v1/apps/{app}/databases", d.handle("create-app-database", func(ctx context.Context, r *http.Request) (any, error) {
		var req struct{ Tenant string }
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return d.createAppDatabase(ctx, req.Tenant, r.PathValue("app"))
	}))
}

// createAppDatabase creates (tenant, app)'s database: the app must have registered a schema (an unregistered
// app has no tables to make, and a typo must not create databases). Create is idempotent and resumable.
func (d *daemon) createAppDatabase(ctx context.Context, tenant, app string) (control.Database, error) {
	if !pg.ValidTenant(tenant) || !pg.ValidName(app) {
		return control.Database{}, errBadRequest("tenant must be a tenant id and app a database name")
	}
	if err := d.checkRegistered(ctx, app); err != nil {
		return control.Database{}, err
	}
	return d.ctl.Create(ctx, tenant, app)
}

// checkRegistered answers not_found unless app has registered a schema.
func (d *daemon) checkRegistered(ctx context.Context, app string) error {
	ok, err := d.schemas.AppRegistered(ctx, app)
	if err != nil {
		return err
	}
	if !ok {
		return &apiError{"not_found", http.StatusNotFound, "app " + app + " has no registered schema: PUT /v1/apps/" + app + "/schema first", false}
	}
	return nil
}

// resolveApp is resolve for (tenant, app) with create on first use: when the locator says the credential may
// create the missing database (locator.ErrCreatable, naming the credential's tenant), the app must have a
// registered schema (404 otherwise, before any count), the caps must allow (checkCaps), and then it is created
// (createAppDatabase) and resolved again from the primary. Creating runs under the
// resolve deadline; if that passes, the create is recorded and resumes (the lifecycle controller finishes
// pending creates), and the client's next resolve follows it.
func (d *daemon) resolveApp(ctx context.Context, req locator.Request) (locator.Route, error) {
	rt, err := d.loc.Resolve(ctx, req)
	if !req.Create || !errors.Is(err, locator.ErrCreatable) {
		return rt, err
	}
	select { // its own small bulkhead: a first-use storm must not fill the resolve slots or an agent's lifecycle ones
	case d.createSem <- struct{}{}:
		defer func() { <-d.createSem }()
	default:
		d.createShed.Add(1)
		return locator.Route{}, &apiError{"shed", http.StatusServiceUnavailable, "too many databases being created on first use; nothing created, retry after Retry-After", true}
	}
	if err := d.checkRegistered(ctx, req.Name); err != nil {
		return locator.Route{}, err
	}
	if err := d.checkCaps(ctx, rt.Tenant); err != nil {
		return locator.Route{}, err
	}
	if _, err := d.createAppDatabase(ctx, rt.Tenant, req.Name); err != nil {
		return locator.Route{}, err
	}
	d.log.Info("app database created on first use", "event", "CONTROLD.APP_DATABASE_CREATED", "db", rt.DB)
	req.Tenant = rt.Tenant
	req.MinEpoch = max(req.MinEpoch, 1) // read the primary: a replica may not have the new row yet
	return d.loc.Resolve(ctx, req)
}

// canonicalSchema is the registry's check before a PUT stores a text (daemon.checkSchema): a valid schema in
// fingerprint form (docexec.CanonicalText), so a stored text always creates databases exactly as the
// TypeScript backend would and every agent accepts it.
func canonicalSchema(_ string, text []byte) error { return docexec.CanonicalText(string(text)) }
