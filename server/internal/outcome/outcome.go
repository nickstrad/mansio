// Package outcome names what happened to one request attempt. It is the single list of outcome codes used by
// the actor result classifier, the retry policies (which outcomes to retry), the request records and the
// network client (internal/client), which parses the front's wire codes with Parse. New codes are appended:
// a code's number never changes.
package outcome

import (
	"fmt"
	"strings"
)

// Code is the outcome of one attempt.
type Code uint8

const (
	OK            Code = iota
	Shed               // the database's mailbox or read queue was full
	StmtTimeout        // the statement timeout interrupted SQLite
	Expired            // the request's deadline passed while it was queued; it never ran
	ClientTimeout      // the client stopped waiting for a reply
	GenFull            // the load generator had no free worker; never sent
	Aborted            // rolled back because another write in its batch timed out
	Closed             // the database was closed
	Error              // any other SQLite or driver error
	Unavailable        // a dependency (storage, placement) was down; nothing ran (wire code "unavailable", RI-10)
	numCodes
)

var names = [numCodes]string{"ok", "shed", "stmt_timeout", "expired", "client_timeout", "gen_full", "aborted", "closed", "error", "unavailable"}

func (c Code) String() string {
	if c < numCodes {
		return names[c]
	}
	return fmt.Sprintf("outcome(%d)", c)
}

// Codes lists every code in order.
func Codes() []Code {
	out := make([]Code, numCodes)
	for i := range out {
		out[i] = Code(i)
	}
	return out
}

// Parse returns the code named s (case-insensitive).
func Parse(s string) (Code, bool) {
	for i, n := range names {
		if strings.EqualFold(s, n) {
			return Code(i), true
		}
	}
	return 0, false
}

// Set is a set of codes.
type Set uint16

// SetOf returns the set holding codes.
func SetOf(codes ...Code) Set {
	var s Set
	for _, c := range codes {
		s |= 1 << c
	}
	return s
}

// All holds every failure code (everything except OK).
var All = SetOf(Codes()...) &^ SetOf(OK)

// Has reports whether c is in s.
func (s Set) Has(c Code) bool { return c < numCodes && s&(1<<c) != 0 }

// ParseSet parses names into a set; "any" means every failure code.
func ParseSet(names []string) (Set, error) {
	var s Set
	for _, n := range names {
		if strings.EqualFold(n, "any") {
			s |= All
			continue
		}
		c, ok := Parse(n)
		if !ok {
			return 0, fmt.Errorf("unknown outcome %q (want one of %v or \"any\")", n, Codes())
		}
		s |= SetOf(c)
	}
	return s, nil
}
