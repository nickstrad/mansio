// Package ir is the IR protocol v1 wire model (ir/spec/v1.md): the JSON value model, the strict request
// decoder, the canonical encoding, the limits and the protocol's error values.
//
// Role: domain, pure. No I/O, no logging, no clock, no randomness. It is a port of the TypeScript reference
// (core/ir/{json,decode,types,errors}.ts); where the two could differ, it reproduces the TypeScript
// behaviour, because the shared vectors (ir/vectors/) and the TS compiler are the oracle (ADR 0010).
//
// JSON values are the Go values nil, bool, float64, string, []any and *Object. An *Object keeps
// ECMAScript own-property order (array-index names first, ascending; then insertion order), because the
// TypeScript code iterates objects in that order and reports the first failure it meets. Every number is a
// float64, as in JavaScript.
//
// Error module: none. A failure here is the answer to a request (bad input), not an operational error, so
// it is a value: *Error, carrying the wire tag, the HTTP status, the operation index and the structured
// fields of spec §8. Callers return it to the client as is; they never wrap it with errs.
package ir
