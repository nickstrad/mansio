// Package det holds the determinism seams (IR protocol plan §6.4, DST-0): Clock, Rand and IDGen. Every package
// that decides state from time, randomness or generated ids takes them from here instead of calling time.Now,
// time.After, context.WithTimeout, math/rand's package-level functions or crypto/rand directly, so a
// deterministic simulation (DST-1, DST-2) can drive the whole system from one seed with simulated time.
//
// Real implementations: System (the wall clock and runtime timers), SystemRand (the runtime's generator) and
// SystemIDs (a random process prefix plus a counter). Seeded and simulated ones: NewRand (a seeded PCG) and
// NewIDs (a prefix drawn from a Rand), and dettest.Clock (a manual clock whose Advance fires timers in order).
//
// The rule (docs/coding-style.md §4) is enforced by lint_test.go over the packages it lists.
//
// A domain package: no I/O beyond reading the wall clock and the runtime's entropy in the System
// implementations. No error module: nothing here fails.
package det
