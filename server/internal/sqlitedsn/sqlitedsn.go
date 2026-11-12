// Package sqlitedsn builds the modernc.org/sqlite connection URI for a file path. It is the one place a DSN is
// built: connection strings are built in one place, never by concatenation. Pure: no I/O.
package sqlitedsn

import (
	"net/url"
	"strings"
)

// Build returns the "file:" URI that opens path with foreign keys enforced and then each pragma applied on
// connect (a declared REFERENCES that SQLite ignores is a lie the application trusts). A pragma is
// written as the driver expects it, e.g. "journal_mode(WAL)" or "busy_timeout(5000)".
//
// The path is percent-escaped: in a file: URI a raw '#' starts a fragment and a raw '?' starts the query, so an
// unescaped path containing either opens a different file.
//
//	pre: pragmas come from code, never from requests (they are not escaped)
func Build(path string, pragmas ...string) string {
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath()}
	u.RawQuery = "_pragma=" + strings.Join(append([]string{"foreign_keys(1)"}, pragmas...), "&_pragma=")
	return u.String()
}

// ReadOnly is Build for a file that must not change (mode=ro): no write, no checkpoint, no new file. With
// immutable, SQLite also takes no locks and reads no -wal or -shm (immutable=1): only for a file nothing else
// has open and whose content is all in the main file, such as a downloaded generation.
//
//	pre: pragmas come from code, never from requests (they are not escaped)
func ReadOnly(path string, immutable bool, pragmas ...string) string {
	dsn := Build(path, pragmas...) + "&mode=ro"
	if immutable {
		dsn += "&immutable=1"
	}
	return dsn
}
