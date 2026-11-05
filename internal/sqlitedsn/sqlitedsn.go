// Package sqlitedsn builds the modernc.org/sqlite connection URI for a file path. It is the one place a DSN is
// built (SQL G-2: build connection strings in one place, never by concatenation). Pure: no I/O.
package sqlitedsn

import (
	"net/url"
	"strings"
)

// Build returns the "file:" URI that opens path with each pragma applied on connect. A pragma is written as the
// driver expects it, e.g. "journal_mode(WAL)" or "busy_timeout(5000)".
//
// The path is percent-escaped: in a file: URI a raw '#' starts a fragment and a raw '?' starts the query, so an
// unescaped path containing either opens a different file.
//
//	pre: pragmas come from code, never from requests (they are not escaped)
func Build(path string, pragmas ...string) string {
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath()}
	if len(pragmas) > 0 {
		u.RawQuery = "_pragma=" + strings.Join(pragmas, "&_pragma=")
	}
	return u.String()
}
