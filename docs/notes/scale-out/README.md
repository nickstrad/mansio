# scale-out

What limits mansio past one process and one machine, and what the options cost. Design documents:
`docs/architecture/scaling_issues.md` (what won't scale), `docs/architecture/scale-out-direct-actors.md`
(option 4), and the planes and lanes datasets under `docs/pages/`. Measured evidence from the sibling Go proof
of concept: `../mansio-server/docs/notes/` (`sqlbench/`, `coldload/`).

## Entries (newest first)

- 2026-11-05 [Go greenfield proposal](../../architecture/scale-out-go-greenfield.md): yes to a Go backend on
  the mansio-server actor model plus option 4's control plane, gated on an embedded-JS handler spike. Better
  Auth scrypt hashes can be verified in Go (read from source, not tested).

- [2026-11-05 mansio-server comparison](2026-11-05-mansio-server-comparison.md): each scaling issue mapped to
  what mansio-server measured, what carries over to mansio, and the handler-in-transaction constraint that
  keeps a Go data plane from being a drop-in.
