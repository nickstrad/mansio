# ADR 0010 — The compiler is ported to Go; the TypeScript compiler stays as the oracle

- **Status:** accepted
- **Date:** 2026-11-05
- **Deciders:** [`docs/plans/ir-protocol.md`](../plans/ir-protocol.md) §9; recorded by IR-0
- **Related:** [ADR 0009](0009-ir-is-the-app-wire.md), [ADR 0005](0005-text-storage-contract.md)
- **Implements:** `ir/vectors/`, `scripts/ir/gen-vectors.ts`, `scripts/ir/gen-compile-corpus.ts`,
  `tests/unit/ir_vectors_test.ts`

## Context

The Go backend needs schema validation, the SQL compiler and row decoding (`core/schema.ts`, `core/compile/*`,
~550 lines). Two implementations drift unless something holds them together, and existing database files must
keep opening (same DDL, same `_platform_schema` fingerprint).

## Decision

- Go gets its own port (`internal/docschema`, `internal/doccompile`, SQLite only). It does not call TypeScript
  at run time.
- `core/` stays the **oracle**. Where the two disagree, TS is right until a vector says otherwise.
- The contract between them is data, not code: the shared vectors in `ir/vectors/*.json` (decode, validation,
  schema, compile, execute) and the generated differential corpus `ir/vectors/generated/compile-corpus.jsonl`.
  Compile SQL and schema fingerprints in the vectors are golden output of `core/`; every other expectation is
  written by hand. The TS runner fails if a committed file no longer matches its generator.
- Every bug fix in either language adds a vector.

## Consequences

- A change to `core/compile` output is a protocol change: regenerate the vectors, and the Go runner must
  follow. `GENERATED_SQL.txt` stays the human-readable canary.
- Go must reproduce `JSON.stringify` of the schema byte for byte (fingerprint), ECMAScript number formatting
  (canonical JSON, RFC 8785), and `core/schema.ts`'s validation on/off points.
