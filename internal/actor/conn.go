package actor

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/sqlitedsn"

	"modernc.org/sqlite" // also registers the "sqlite" driver (pure Go, no cgo)
)

// executor is the actor's port to one database connection. sqliteConn is the real adapter; tests use fakes
// with fault switches.
type executor interface {
	// exec runs one request and reports rows (read) or rows affected (write). It never panics on SQL errors.
	exec(ctx context.Context, r *Request) Result
	// run executes a statement with no arguments and no result rows (BEGIN, COMMIT, ROLLBACK, DDL).
	run(ctx context.Context, q string) error
	// backup copies the database to dst, which must not exist, from one read transaction.
	backup(ctx context.Context, dst string) error
	close()
}

// opener opens one connection to path.
type opener func(path string, cfg Config, readOnly bool) (executor, error)

// sqliteConn is one SQLite connection with a least-recently-used prepared-statement cache and reusable scan
// buffers. Used by one goroutine at a time.
type sqliteConn struct {
	db    *sql.DB
	c     *sql.Conn
	limit int
	lru   *list.List               // of *cached, most recent at the front
	stmts map[string]*list.Element // SQL → element in lru
	first any                      // scan buffers, reused across reads
	raw   []sql.RawBytes
	ptrs  []any
}

type cached struct {
	sql  string
	stmt *sql.Stmt
}

func openSQLite(path string, cfg Config, readOnly bool) (executor, error) {
	ctx := context.Background()
	dsn := dsn(path, cfg, readOnly)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errs.Wrap("actor", err, "cannot open database %s", path)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	c, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, errs.Wrap("actor", err, "cannot open database %s", path)
	}
	k := &sqliteConn{db: db, c: c, limit: cfg.StmtCache, lru: list.New(), stmts: map[string]*list.Element{}}
	if !readOnly {
		// SQLite keeps the old mode when it cannot switch (e.g. to WAL while another connection holds the file
		// in rollback mode), so check that the writer got the mode we asked for. Readers share it.
		var mode string
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			k.close()
			return nil, errs.Wrap("actor", err, "cannot read journal_mode of %s", path)
		}
		if !strings.EqualFold(mode, cfg.JournalMode) {
			k.close()
			return nil, errs.Wrap("actor", nil, "database %s stayed in journal_mode %s; wanted %s (another connection may hold it)",
				path, mode, cfg.JournalMode)
		}
	}
	return k, nil
}

// dsn is the driver's URI for path (sqlitedsn escapes it and always enforces foreign keys).
func dsn(path string, cfg Config, readOnly bool) string {
	pragmas := []string{
		fmt.Sprintf("busy_timeout(%d)", cfg.BusyTimeout.Milliseconds()),
		fmt.Sprintf("journal_mode(%s)", cfg.JournalMode),
		fmt.Sprintf("synchronous(%s)", cfg.Synchronous),
		fmt.Sprintf("cache_size(-%d)", cfg.CacheKiB),
	}
	if readOnly {
		pragmas = append(pragmas, "query_only(1)")
	}
	return sqlitedsn.Build(path, pragmas...)
}

func (k *sqliteConn) close() {
	for _, e := range k.stmts {
		e.Value.(*cached).stmt.Close()
	}
	k.c.Close()
	k.db.Close()
}

// backupStepPages is how many pages one backup step copies (16 MiB at 4 KiB pages); ctx is checked between steps.
var backupStepPages int32 = 4096 // a var so tests can force many steps

// backuper is the modernc driver connection's backup hook (sqlite3_backup_*), reached through sql.Conn.Raw.
type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

// backup copies the database to dst with the online backup API. It holds one read transaction across every
// step, so the copy is one consistent snapshot even while another connection commits (in WAL mode the writer is
// not blocked; the backup does not restart). dst is created exclusively first, so an existing file is an error,
// never overwritten.
func (k *sqliteConn) backup(ctx context.Context, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close() // an empty file is a valid empty database for the backup to fill
	if _, err := k.c.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer k.c.ExecContext(context.Background(), "ROLLBACK") //nolint:errcheck // read-only transaction
	// A deferred BEGIN takes no snapshot until the first read; pin it now so every step reads the same one.
	if _, err := k.c.ExecContext(ctx, "SELECT count(*) FROM sqlite_schema"); err != nil {
		return err
	}
	return k.c.Raw(func(dc any) error {
		b, ok := dc.(backuper)
		if !ok {
			return fmt.Errorf("driver connection %T has no backup API", dc)
		}
		// The copy is a temp file made durable by its upload: no journal, no fsync per step.
		bk, err := b.NewBackup(sqlitedsn.Build(dst, "synchronous(OFF)", "journal_mode(OFF)"))
		if err != nil {
			return err
		}
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				bk.Finish()
				return err
			}
			if more, err = bk.Step(backupStepPages); err != nil {
				bk.Finish()
				return err
			}
		}
		return bk.Finish()
	})
}

// stmt returns a prepared statement for q, preparing it and evicting the least recently used one if needed.
func (k *sqliteConn) stmt(ctx context.Context, q string) (*sql.Stmt, error) {
	if e, ok := k.stmts[q]; ok {
		k.lru.MoveToFront(e)
		return e.Value.(*cached).stmt, nil
	}
	s, err := k.c.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	k.stmts[q] = k.lru.PushFront(&cached{sql: q, stmt: s})
	if k.lru.Len() > k.limit {
		old := k.lru.Remove(k.lru.Back()).(*cached)
		delete(k.stmts, old.sql)
		old.stmt.Close()
	}
	return s, nil
}

func (k *sqliteConn) run(ctx context.Context, q string) error {
	s, err := k.stmt(ctx, q)
	if err != nil {
		return err
	}
	_, err = s.ExecContext(ctx)
	return err
}

func (k *sqliteConn) exec(ctx context.Context, r *Request) Result {
	s, err := k.stmt(ctx, r.SQL)
	if err != nil {
		return Result{Err: err}
	}
	if r.Write {
		res, err := s.ExecContext(ctx, r.Args...)
		if err != nil {
			return Result{Err: err}
		}
		n, _ := res.RowsAffected()
		return Result{Rows: n}
	}
	rows, err := s.QueryContext(ctx, r.Args...)
	if err != nil {
		return Result{Err: err}
	}
	defer rows.Close()
	if r.MaxRows > 0 {
		return collect(rows, r.MaxRows)
	}
	var out Result
	for rows.Next() {
		if err := rows.Scan(k.scanTargets(rows)...); err != nil {
			return Result{Err: err}
		}
		if out.Rows == 0 {
			out.First, _ = k.first.(int64)
		}
		out.Rows++
	}
	out.Err = rows.Err()
	return out
}

// collect reads up to limit rows of copied values; one more row fails the read with ErrTooManyRows.
//
//	post: res.Err == nil ⇒ len(res.Values) == res.Rows <= limit and every row has len(res.Columns) values
func collect(rows *sql.Rows, limit int) Result {
	cols, err := rows.Columns()
	if err != nil {
		return Result{Err: err}
	}
	out := Result{Columns: cols}
	for rows.Next() {
		if len(out.Values) == limit {
			return Result{Err: ErrTooManyRows, Rows: int64(limit) + 1, Columns: cols}
		}
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil { // database/sql copies []byte into a *any destination
			return Result{Err: err}
		}
		if len(out.Values) == 0 {
			out.First, _ = row[0].(int64)
		}
		out.Values = append(out.Values, row)
	}
	out.Rows = int64(len(out.Values))
	out.Err = rows.Err()
	return out
}

// scanTargets returns reusable destinations: the first column as a value (for Result.First), the rest as raw
// bytes (no copy).
func (k *sqliteConn) scanTargets(rows *sql.Rows) []any {
	cols, _ := rows.Columns()
	n := len(cols)
	if cap(k.ptrs) < n {
		k.raw = make([]sql.RawBytes, n)
		k.ptrs = make([]any, n)
	}
	k.ptrs = k.ptrs[:n]
	for i := range n {
		if i == 0 {
			k.ptrs[0] = &k.first
		} else {
			k.ptrs[i] = &k.raw[i]
		}
	}
	return k.ptrs
}

// Seed creates the database at path: it runs ddl and then fill (which inserts through exec) in one transaction.
// It is atomic: the database is built under a temporary name in the same directory and renamed into place
// only after it committed and closed, so path either holds the complete database or is untouched. If path
// already exists it is replaced.
func Seed(path string, cfg Config, ddl string, fill func(exec func(q string, args ...any) error) error) error {
	tmp := path + ".seeding"
	removeDB(tmp) // left by a process killed while seeding
	if err := seedFile(tmp, cfg.WithDefaults(), ddl, fill); err != nil {
		removeDB(tmp)
		return errs.Wrap("actor", err, "cannot seed %s; nothing was written", path).With("path", path)
	}
	if err := os.Rename(tmp, path); err != nil {
		removeDB(tmp)
		return errs.Wrap("actor", err, "cannot move seeded database into place; nothing was written").With("path", path)
	}
	return nil
}

// seedFile builds the database at path; the caller cleans up on error.
func seedFile(path string, cfg Config, ddl string, fill func(exec func(q string, args ...any) error) error) error {
	e, err := openSQLite(path, cfg, false)
	if err != nil {
		return err
	}
	k := e.(*sqliteConn)
	defer k.close() // the last close checkpoints the WAL into the file and removes the sidecars
	ctx := context.Background()
	if err := k.run(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("BEGIN: %w", err)
	}
	fail := func(what string, err error) error {
		_ = k.run(ctx, "ROLLBACK")
		return fmt.Errorf("%s: %w", what, err)
	}
	if _, err := k.c.ExecContext(ctx, ddl); err != nil { // multi-statement script: not cacheable
		return fail("schema", err)
	}
	if fill != nil {
		exec := func(q string, args ...any) error {
			return k.exec(ctx, &Request{SQL: q, Args: args, Write: true}).Err
		}
		if err := fill(exec); err != nil {
			return fail("fill", err)
		}
	}
	if err := k.run(ctx, "COMMIT"); err != nil {
		return fail("commit", err)
	}
	return nil
}

// removeDB removes a database file and every sidecar SQLite may leave next to it.
func removeDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(path + suffix)
	}
}

var errAlreadyStarted = errors.New("actor: already started")
