// Package pg is the Postgres adapter for owner.Placement: the placement authority that hands out epochs.
// Error module "placement".
//
// Integration code, kept thin: it validates input at the edge, runs one embedded query per port method
// (queries/*.sql, one query per file), maps results and errors, and guards every call with a deadline and a circuit
// breaker. The decisions (when to create, move or flip) belong to owner and the control plane; the invariants (epochs
// never decrease; the owner changes only with an epoch increase; a flip records its moves row) live in the schema
// (schema/*.sql, applied by Migrate and recorded in schema_version).
//
// Error map (callers branch with errors.Is):
//
//	owner.ErrNotFound   Get: no row for the key (an answer)
//	owner.ErrConflict   CAS: the row is not the expected one, nothing changed (an answer)
//	owner.ErrInvalid    bad input refused before anything was sent (key, VM id, epoch range)
//	ErrInvariant        the schema refused the change (trigger, check, unique): a bug in the caller
//	ErrUnknownVM        the owner VM is not registered (RegisterVM first)
//	ErrUnavailable      the breaker is open (nothing sent; RetryAfter hint), or a system failure: timeout,
//	                    lock timeout, connection error. A CAS that fails this way may have applied: re-read
//	                    with Get before deciding. Never matches ErrConflict or ErrNotFound.
//
// Only ErrUnavailable system failures count against the breaker; answers and bad input are successes, and a call
// whose caller gave up is ignored.
package pg
