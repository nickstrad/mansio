// Package sqlitedsn builds the modernc.org/sqlite connection URI for a file path. It is the one place a DSN is
// built (SQL G-5: build connection strings in one place, never by concatenation). Pure: no I/O.
package sqlitedsn

import (
	"net/url"
	"strings"
)

// Build returns the "file:" URI that opens path with foreign keys enforced and then each pragma applied on
// connect (SQL G-13: a declared REFERENCES that SQLite ignores is a lie the application trusts). A pragma is
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
