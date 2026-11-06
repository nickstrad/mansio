# STATE-front: the agent's network front (tickets, admission, executor port). ARCHIVED 2026-11-05

Owner: front agent. Branch `front` (worktree `/Users/nick/Software/mansio-server-wt/front`), merged to `main`.
Contract 3 in `shared_state.md` (2026-11-05 10:15). Findings: `knowledge/front/`.

## Goal

Give the actor-per-database process a client-facing listener: Ed25519 tickets (option 4 §3.1), per-tenant and
per-database admission, an `Admitter` port (fencing's `owner` later), an `Executor` port (actor now, handler
runtime later), and a fixed wire-code mapping. Measure the hop.

## Status (newest first)

- 2026-11-05: done. Simplify pass applied; gate green (gofmt, vet, `-race`, `-tags noassert`, e2e, fuzz under the
  bench lock). Merged to main; state archived here.
- 2026-11-05: FuzzVerify found the base64 `\r`/`\n` skip (ticket malleability); fixed with a regression corpus.
- 2026-11-05: measured (`knowledge/front/README.md`): pipeline −7% vs in-process actor for reads; loopback HTTP
  46k reads/s (h1) vs 331k in-process; ticket verify 26 µs, cached 53 ns.
- 2026-11-05: ticket, front, agentd, frontbench built; claims and the actor change posted first.

## Owned files

`docs/state_files/STATE-front.md`, `internal/ticket/`, `internal/front/`, `cmd/agentd/`, `cmd/frontbench/`,
`configs/agentd/`, `scripts/front/`, `knowledge/front/`, the `front` section of `Makefile`. Additive edits
(posted): `internal/actor/{actor,conn}.go` + `rows_test.go` (MaxRows), one line in `knowledge/index.md`.

## APIs others rely on

- `front.Admitter { Admit(db string, epoch uint64) (release func(), err error) }`; sentinels `front.ErrWrongOwner`,
  `front.ErrFenced`, `front.ErrShed`. `front.StaticAdmitter` is the stand-in.
- `front.Executor { Exec(ctx, Call) (Reply, error) }`; `Call` carries `Tenant, DB, Epoch, Gen, RequestID, Kind, SQL,
  Args, Deadline, StmtTimeout, MaxRows`. Contract suite: `fronttest.RunExecutorSuite`.
- `front.Host { Lookup(ctx, db string) (*actor.Actor, error) }`; `front.ErrUnknownDB`; `front.StaticHost`.
- `ticket.Mint`, `ticket.Verifier`, `ticket.Cache`, `ticket.KeyRing`, `ticket.ParsePublicKeys`.

## API changes (newest first)

- 2026-11-05: `front.Call` gained `Epoch`, `Gen`. `front.StaticAdmitter` added.
- 2026-11-05: `actor.Request.MaxRows`, `actor.Result.{Columns,Values}`, `actor.ErrTooManyRows` (additive; MaxRows 0 =
  old behavior).
