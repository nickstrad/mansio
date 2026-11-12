// Package locator is the control plane's locator and ticket issuer: resolve(tenant, database, principal) → {endpoint,
// epoch, gen, ticket, TTL}. Domain code: no I/O and no logging. It reaches the world through the ports in ports.go:
// the placement store (Postgres, a replica or the primary), the API-key store, and the ticket signer.
//
// Resolve authenticates the API key (hashed at rest; checked once per ticket, cached briefly), checks the
// key's tenant, reads the placement through a bounded cache, and mints a ticket for the owner VM at the row's
// epoch and generation.
//
// # Staleness is safe; the cache only saves work
//
// A stale answer names an old VM or epoch: the agent there refuses it (wrong_owner / fenced) and the client
// re-resolves with min_epoch = last seen + 1, which bypasses the cache and reads the primary. The cache is
// invalidated by placement notifications: Invalidate for one key, Reset when the listener (re)connects (LISTEN first,
// then drop everything cached, then apply what arrives), Untrust when it is lost (the cache is bypassed until the
// next Reset). A lost notification costs one wrong_owner and one re-resolve. InvalidateState applies a registry state
// notification (a create or move finished): it tombstones the key whatever its epoch, since that flip does not change
// the epoch.
//
// # Load
//
// Loads are single-flight per (database, primary): a re-resolve storm after a move turns into one query. A
// load runs under its own deadline, so a caller that gives up does not cancel it for the others, and it
// cannot outlive LoadTimeout. A row that is MOVING (or a database still being created) answers ErrMoving with
// a retry hint (MovingRetry); so does a row already ACTIVE on a new owner whose Claim the registry has not
// confirmed (ClaimRetry, short): that owner would answer wrong_owner at the current epoch. A suspended database
// answers ErrSuspended.
//
// # Unknown API keys
//
// Unknown key ids are never cached, so each would cost a primary read: an id not in the key cache is first
// charged to the request's source (Config.Limiter, internal/ratelimit); an empty bucket answers ErrRateLimited.
package locator
