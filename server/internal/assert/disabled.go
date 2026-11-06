//go:build noassert

package assert

// Enabled is true unless built with -tags noassert.
const Enabled = false
