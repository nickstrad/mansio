# Edge: refuse before reading the body (B3, 2026-11-10)

Code: `internal/edge/proxy.go` (`handle`, `forward`), config `max_in_flight` in `cmd/edge/config.go`. Test:
`TestRefusalsCostNoBodyRead` (measured there: 20 refusals against a saturated agent and 20 against an open
breaker read 0 body bytes; before, each read its whole body).

- Before the body is read the edge now checks, in order: draining, its own `max_in_flight` (default
  2 × `agent_in_flight`; over it → `shed`), the ticket, the route, whether the agent's bulkhead is full (`shed`)
  and whether its breaker is open (`unavailable` with the cool-down as `Retry-After`).
- The check only looks; it holds nothing while the client sends. The plan's wording was "take the bulkhead slot
  before reading the body"; a slot (or a half-open breaker's single probe permit) held across a body read would
  let a slow sender pin it for `body_read_timeout`, which is the hazard B2 removes on the agent. `forward` takes
  the slot and the permit once the body is in memory and can still refuse if either went in between.
- Memory ceiling for request bodies: `max_in_flight × max_body` (8 GiB with the defaults 8192 × 1 MiB); size
  `max_in_flight` to the machine.
- Metrics: `mansio_edge_refusals_total{reason="max_in_flight"}` and the `edge_*` queues in
  [the per-queue families](2026-11-10-queue-metrics.md).

Precision on "reads nothing": the edge's handler reads and buffers none of the body. Over HTTP/1.1 Go's
`net/http` then discards up to 256 KiB of an unread request body before it writes the reply (larger bodies
close the connection instead); that streams to nowhere, takes no slot and no edge memory, and for a client
that already sent its body it is immediate. A sender that stalls waits for its own answer until
`body_read_timeout`. HTTP/2 has no such step. The same holds for agentd's draining refusal on `/v1/ops` (B6).
