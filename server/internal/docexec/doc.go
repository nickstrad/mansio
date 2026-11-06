// Package docexec executes decoded IR requests (ir/spec/v1.md §6–§7) on a database actor's connection: a
// port of platform/ir/executor.ts and the execution rules of core/ir/semantics.ts ($ref resolution, bodies,
// expect). It is the oracle's twin: the shared execute vectors run against it on a real actor.
//
// What it decides, per request (spec §6.1): the fingerprint check against _platform_schema (Check, before the
// actor's duplicate check), then each operation in order: table lookup, $ref resolution into copies (the
// decoded request is never written to, so it can be replayed or compared), body validation, expect, the
// insert's client-id existence check, the write. Selects see earlier operations' writes; one clock reading
// (Tx.Now) is every insert's _creationTime; server ids come from an injected det.IDGen in operation order.
//
// How it runs: Call adapts a request to an actor closure (actor.Request.Check and Fn). A mutation is a write
// closure, inside its savepoint in the group-commit batch, with the actor's request-id record holding the
// canonical JSON of the result array (spec §7); a query is a read closure in one snapshot. A new, empty
// database has no schema: Check answers ErrUninitialized and the caller runs Init (a write closure) with the
// registered schema, then retries.
//
// Role: domain. It does no I/O of its own and does not log: every statement goes through Tx, which the
// actor's *actor.Tx satisfies. Values are only ever bind parameters (internal/doccompile).
//
// Errors: a protocol failure is a *Failure whose Err is the wire error (ir.Error: tag, fields, op); a failed
// statement is a *Failure with Err a StorageError at its operation and Cause the statement's error, for the
// caller to classify (statement timeout, aborted batch). ErrUninitialized is a sentinel. No error module: the
// caller (internal/front) renders failures on the wire.
package docexec
