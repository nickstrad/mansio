package pg

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// Schema migrations: numbered, transactional files, applied once and recorded in schema_version (SQL G-3).
// Each file wraps itself in begin … commit and inserts its own version, so psql -f applies it the same way.
//
//go:embed schema/*.sql
var schemaFS embed.FS

// migration is one embedded schema file.
type migration struct {
	version int
	name    string
	sql     string
}

// migrations returns the embedded files in order. Their names must be NNNN_name.sql numbered 1, 2, 3, …
// with no gap: a gap or duplicate is a build bug, reported as an error.
func migrations() ([]migration, error) {
	names, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(names))
	for i, path := range names { // Glob returns names sorted
		name := strings.TrimPrefix(path, "schema/")
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || len(num) != 4 || err != nil || v != i+1 {
			return nil, fmt.Errorf("schema file %s: want %04d_<name>.sql", name, i+1)
		}
		b, err := schemaFS.ReadFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(b)})
	}
	return out, nil
}

// migrateLock is the advisory lock key Migrate holds, so two processes starting together do not both try
// the same file. It only saves work: a second apply of a file fails on its own DDL and rolls back (SQL G-11).
const migrateLock = 0x7473_6462_706c_6163 // "tsdbplac"

// Migrate applies every embedded schema file newer than the database's schema_version, in order, each in
// its own transaction, and returns the version the database is at. Applying an up-to-date database is a
// no-op. A database newer than this binary is an error (refuse to run against an unknown schema).
func (p *Placement) Migrate(ctx context.Context) (int, error) {
	ms, err := migrations()
	if err != nil {
		return 0, errs.Wrap(module, err, "cannot migrate the placement schema: bad embedded files (a build bug)")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return 0, errs.Wrap(module, err, "cannot migrate the placement schema: no connection; nothing changed, safe to retry")
	}
	defer conn.Release()
	pc := conn.Conn().PgConn()
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", int64(migrateLock)); err != nil {
		return 0, errs.Wrap(module, err, "cannot migrate the placement schema: lock; nothing changed, safe to retry")
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.Background(), time.Second)
		defer ucancel()
		if _, err := conn.Exec(uctx, "select pg_advisory_unlock($1)", int64(migrateLock)); err != nil {
			conn.Conn().Close(uctx) // drop the session, and the lock with it
		}
	}()
	version := func() (int, error) {
		var exists bool
		if err := conn.QueryRow(ctx, "select to_regclass('schema_version') is not null").Scan(&exists); err != nil || !exists {
			return 0, err
		}
		var v int
		err := conn.QueryRow(ctx, "select coalesce(max(version), 0) from schema_version").Scan(&v)
		return v, err
	}
	cur, err := version()
	if err != nil {
		return 0, errs.Wrap(module, err, "cannot read the placement schema version; nothing changed, safe to retry")
	}
	if cur > len(ms) {
		return cur, errs.Wrap(module, nil, "placement schema is at version %d, newer than this binary's %d; refusing to run", cur, len(ms))
	}
	for _, m := range ms[cur:] {
		// Simple protocol: a file is several statements, and its begin … commit makes it all-or-nothing.
		if _, err := pc.Exec(ctx, m.sql).ReadAll(); err != nil {
			pc.Exec(ctx, "rollback").ReadAll() // leave no aborted transaction on the pooled session
			return cur, errs.Wrap(module, err, "cannot apply placement schema %s; it rolled back, the schema stays at version %d", m.name, cur)
		}
		if cur, err = version(); err != nil {
			return cur, errs.Wrap(module, err, "cannot read the placement schema version after %s; it may have applied, safe to rerun", m.name)
		}
		if cur != m.version {
			return cur, errs.Wrap(module, nil, "placement schema %s applied but schema_version is %d, want %d (a bug in the file)", m.name, cur, m.version)
		}
	}
	return cur, nil
}
