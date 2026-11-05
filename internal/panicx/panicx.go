// Package panicx carries a panic from the goroutine where it happened to the one that should handle it.
//
// A goroutine recovers with From(recover()), hands the *Panic to its parent, and the parent re-panics with it.
// The top level then reports the original stack, not the parent's.
package panicx

import (
	"fmt"
	"runtime/debug"
)

// Panic is a recovered panic and the stack of the goroutine that panicked.
type Panic struct {
	Value any
	Stack string
}

func (p *Panic) Error() string { return fmt.Sprintf("panic: %v", p.Value) }

// From wraps a recovered value, or returns nil for nil. A *Panic passes through, keeping its original stack.
// Call it in the deferred function that recovered, so the stack is the panicking goroutine's.
func From(v any) *Panic {
	switch p := v.(type) {
	case nil:
		return nil
	case *Panic:
		return p
	default:
		return &Panic{Value: v, Stack: string(debug.Stack())}
	}
}
