// Package docschema is the document schema model: node kinds, validation of document values, the
// declaration rules, the registered JSON form and its fingerprint (ir/spec/v1.md §2, §5.2). It ports
// core/schema.ts and core/ir/schema.ts; the TypeScript is the oracle (ADR 0010), so wording, check order
// and the order of members in the fingerprint text are reproduced exactly.
//
// Role: domain, pure. No I/O, no logging. Values are ir JSON values (nil, bool, float64, string, []any,
// *ir.Object).
//
// Error module: none. Every rejection is an *ir.Error value (SchemaError or CompileError) for the caller to
// answer with; nothing here is an operational failure.
package docschema
