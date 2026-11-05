# STATE-front — the agent's network front (tickets, admission, executor port)

Owner: front agent. Branch `front`, worktree `/Users/nick/Software/ts-db-go-wt/front`. Contract 3 in
`shared_state.md` (2026-11-05 10:15).

## Goal

Give the actor-per-database process a client-facing listener: Ed25519 tickets (option 4 §3.1), per-tenant and
per-database admission, an `Admitter` port (fencing's `owner` later), an `Executor` port (actor now, handler
runtime later), and a fixed wire-code mapping. Measure the hop.

## Plan

1. Additive `internal/actor` change: `Request.MaxRows`, `Result.Columns/Values`, `ErrTooManyRows` (posted).
2. `internal/ticket` (+ `tickettest`): encoding, mint, pure verify, key ring.
3. `internal/front`: request decoder + SQL guard, limiter (pure) + admission, wire mapping, pipeline, HTTP.
   `fronttest`: fake Admitter/Executor/Clock, Executor contract suite.
4. `cmd/agentd`, `configs/agentd/`.
5. `cmd/frontbench` (seed, keygen, run inproc/http h1/h2c).
6. Fuzz, coverage, race, noassert, e2e; measurements under the bench lock; `knowledge/front/`.

## Status (newest first)

- 2026-11-05: started; claims and actor change posted to shared_state.md.

## Owned files

`STATE-front.md`, `internal/ticket/`, `internal/front/`, `cmd/agentd/`, `cmd/frontbench/`, `configs/agentd/`,
`knowledge/front/`, the `front` section of `Makefile`. Additive edits (posted): `internal/actor/{actor,conn}.go`
(MaxRows), one line in `knowledge/index.md`.

## APIs others rely on

- `front.Admitter { Admit(db string, epoch uint64) (release func(), err error) }`; sentinels
  `front.ErrWrongOwner`, `front.ErrFenced`.
- `front.Host { Lookup(ctx, db string) (*actor.Actor, error) }`; `front.ErrUnknownDB`.
- `front.Executor { Exec(ctx, Call) (Reply, error) }`.

## API changes (newest first)

- 2026-11-05: (planned, posted) `actor.Request.MaxRows`, `actor.Result.{Columns,Values}`, `actor.ErrTooManyRows`.
