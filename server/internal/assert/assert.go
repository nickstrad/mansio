// Package assert checks internal contracts: preconditions, postconditions and invariants. A failure is a
// bug in this program, never bad input (validate input and return errors instead).
//
// Assertions are on by default; build with -tags noassert to compile them out. Arguments to That are
// evaluated even when disabled, so guard expensive checks with `if assert.Enabled { ... }`.
package assert

import "fmt"

// That panics with the formatted message if cond is false and assertions are enabled.
func That(cond bool, format string, args ...any) {
	if Enabled && !cond {
		panic(fmt.Sprintf("assertion failed: "+format, args...))
	}
}
