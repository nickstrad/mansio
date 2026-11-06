// Package doccompile compiles document plans and writes to SQL and decodes rows back into documents: a
// port of core/compile/{select,write,ddl,decode,dialect}.ts and core/plan.ts's normalise. SQLite is the
// target; the DuckDB dialect of the read path is kept too, because GENERATED_SQL.txt prints both and must
// stay byte-identical. The TypeScript compiler is the oracle (ADR 0010): SQL text, bind values, the order
// of checks and every error field (including free-text detail) match it exactly, over any input, including
// schemas that ignore the declaration rules (docschema.Load).
//
// Role: domain, pure. No I/O, no logging. Values are only ever bind parameters; identifiers are checked
// (docschema.Identifier), never escaped.
//
// Error module: none. A failure is an *ir.Error value (CompileError, SchemaError or DecodeError).
package doccompile
