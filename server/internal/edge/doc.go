// Package edge is the stateless edge of the scale-out deployment: a thin L7 proxy in front of the agents that routes
// each query on its ticket's vm claim, so agents stay private and only the edge faces clients.
//
//	client ── TLS ──► edge: rate limit (cache miss only) → verify ticket → route on vm → per-agent bulkhead
//	                        and breaker ──► TLS (private CA) ──► agentd /v1/query
//
// The edge keeps no state that matters: verified tickets and per-source buckets are caches it can lose. It does
// no auth lookup, no compile and no Postgres work; the agent still verifies every ticket itself (audience,
// database, permission, epoch). What the edge adds is a cheap filter in front of the 26 µs Ed25519 verify, so a
// flood of garbage tickets costs the attacker's own budget, not every client's.
//
// The decisions are pure and take the time as an argument (gate.go; the per-source limiter is
// internal/ratelimit); the HTTP adapter (proxy.go) does the I/O. Ports: Resolver (VM → agent address:
// StaticRoutes from config, or RegistryRoutes polling controld's GET /v1/agents, registry.go) and
// ticket.KeySet (the issuer's public keys: a key file, or controld's GET /v1/keys through internal/keyfetch).
//
// Error module: "edge". Wire answers reuse internal/front's codes (front.Code); the edge makes only shed,
// unavailable, expired, wrong_owner, ticket_invalid, bad_request and internal itself, and passes every agent answer
// through unchanged (status, body, Retry-After).
package edge
