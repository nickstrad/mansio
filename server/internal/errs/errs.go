// Package errs is the well-formed error type: the module that wrapped it, a message, the cause, when, where (a
// stack), and context.
//
// Every error leaving a package's public API is wrapped once, at that boundary, with the package's module
// name and a one-line message a user can act on. The cause stays reachable through Unwrap, so errors.Is and
// errors.As still match sentinels. The stack is captured once, at the first well-formed layer. The top level
// (cmd) treats an outermost error from a known module as an expected failure and anything else as a bug.
package errs

import (
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// Error is a well-formed error.
type Error struct {
	Module  string         // module boundary that wrapped it, e.g. "vactor"
	Message string         // this layer's message, without the cause
	Inner   error          // the cause; nil if this module found the problem itself
	Time    time.Time      // UTC
	Stack   string         // set only at the origin (first well-formed layer)
	Misc    map[string]any // context: key, path, url, ...
}

// Wrap returns a well-formed error for module. err may be nil.
//
//	pre: module != "" (a programming error otherwise; not checked at run time)
func Wrap(module string, err error, format string, args ...any) *Error {
	e := &Error{Module: module, Message: fmt.Sprintf(format, args...), Inner: err, Time: time.Now().UTC()}
	var inner *Error
	if !errors.As(err, &inner) {
		e.Stack = string(debug.Stack())
	}
	return e
}

// With adds context and returns e.
func (e *Error) With(key string, v any) *Error {
	if e.Misc == nil {
		e.Misc = map[string]any{}
	}
	e.Misc[key] = v
	return e
}

// Error is "message: cause", the standard Go wrapping convention.
func (e *Error) Error() string {
	if e.Inner == nil {
		return e.Message
	}
	return e.Message + ": " + e.Inner.Error()
}

func (e *Error) Unwrap() error { return e.Inner }

// Outermost returns err as *Error only if the outermost error is one; it does not search the chain.
func Outermost(err error) (*Error, bool) {
	e, ok := err.(*Error)
	return e, ok
}

// Layer is one step of an error chain, for logs.
type Layer struct {
	Module  string         `json:"module,omitempty"`
	Message string         `json:"message"`
	Misc    map[string]any `json:"misc,omitempty"`
}

// Chain lists the layers outermost first. A raw error shows only its text, which is how a log shows where
// wrapping stopped.
func Chain(err error) []Layer {
	var out []Layer
	for ; err != nil; err = errors.Unwrap(err) {
		if e, ok := err.(*Error); ok {
			out = append(out, Layer{e.Module, e.Message, e.Misc})
		} else {
			out = append(out, Layer{Message: err.Error()})
		}
	}
	return out
}

// Stack returns the stack captured at the origin, or "".
func Stack(err error) string {
	s := ""
	for ; err != nil; err = errors.Unwrap(err) {
		if e, ok := err.(*Error); ok && e.Stack != "" {
			s = e.Stack
		}
	}
	return s
}
