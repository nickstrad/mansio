// Package schemareg is the app schema registry's shared rules and agentd's client for it. controld stores schemas
// (PUT /v1/apps/{app}/schema, Postgres 0006_app_schemas.sql); agentd fetches one by fingerprint (GET
// /v1/apps/{app}/schemas/{fingerprint}) when a request names it, through Client: a bounded cache, single-flight
// loads, a deadline per fetch and a circuit breaker.
//
// A schema is opaque text here: the fingerprint text of ir/spec/v1.md §2 (JSON.stringify of the defineSchema
// value), and its fingerprint is "sha256:" + the lowercase hex SHA-256 of exactly those bytes. Check verifies
// that and that the text is a JSON object; whether it is a valid schema, and in its canonical (fingerprint)
// form, is internal/docschema's to decide: callers pass that as a Validate hook (controld on register, Client
// on fetch).
//
// check.go and cache.go are pure; client.go is an adapter (HTTP). Error module "schemareg".
//
// Error map (callers branch with errors.Is):
//
//	ErrInvalid      the text, fingerprint or app is malformed, or the text does not hash to the fingerprint
//	ErrNotFound     the registry has no such schema for this app (an answer, not a failure)
//	ErrUnavailable  the registry cannot answer now: the breaker is open, too many fetches in flight, a
//	                timeout, a connection error, a 5xx, or an answer that fails Check (RetryAfter hint)
package schemareg
