package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/ts-db-go/internal/coldload"
	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// Template schema: a small meta table plus items with a 1000-byte random payload (incompressible) and one
// secondary index. The size target is reached by adding rows.
const templateDDL = `
CREATE TABLE meta(k TEXT PRIMARY KEY, v TEXT NOT NULL) STRICT;
CREATE TABLE items(id INTEGER PRIMARY KEY, k INTEGER NOT NULL, name TEXT NOT NULL, payload BLOB NOT NULL) STRICT;
CREATE INDEX items_k ON items(k);
`

const (
	payloadBytes = 1000
	fillSQL      = `WITH RECURSIVE c(i) AS (SELECT ?1 UNION ALL SELECT i + 1 FROM c WHERE i < ?2)
		INSERT INTO items(id, k, name, payload)
		SELECT i, abs(random()) % 1000000, 'name-' || i, randomblob(?3) FROM c`
)

// buildTemplate returns a SQLite file of at least s.Bytes bytes in dir, reusing one that is already at least
// 90% of the target. The file is in WAL mode and fully checkpointed (no -wal side file).
func buildTemplate(dir string, s coldload.SizeClass) (path string, size int64, err error) {
	path = filepath.Join(dir, s.Name+".sqlite")
	if fi, err := os.Stat(path); err == nil && fi.Size() >= s.Bytes*9/10 {
		return path, fi.Size(), nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	vactor.RemoveDB(path)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if cerr := db.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			vactor.RemoveDB(path)
		}
	}()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(templateDDL); err != nil {
		return "", 0, err
	}
	if _, err := db.Exec(`INSERT INTO meta VALUES ('size_class', ?), ('target_bytes', ?), ('created', ?)`,
		s.Name, s.Bytes, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", 0, err
	}
	dbSize := func() (n int64, err error) {
		err = db.QueryRow("SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()").Scan(&n)
		return n, err
	}
	next, perRow := int64(1), int64(payloadBytes+120) // first guess; refined after each chunk
	for {
		cur, err := dbSize()
		if err != nil {
			return "", 0, err
		}
		if cur >= s.Bytes {
			break
		}
		rows := max(1, min((s.Bytes-cur)/perRow, 100_000))
		if _, err := db.Exec(fillSQL, next, next+rows-1, payloadBytes); err != nil {
			return "", 0, err
		}
		next += rows
		if after, err := dbSize(); err == nil && after > cur {
			perRow = max(1, (after-cur)/rows)
		}
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return "", 0, err
	}
	if err := db.Close(); err != nil {
		return "", 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return path, fi.Size(), nil
}
