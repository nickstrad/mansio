// Package sim is the single-node deterministic simulation: one agent's request path (front pipeline → owner table →
// vactor host → real SQLite actors → durable store) driven from one seed, with simulated time and faults, so that
// any failure replays exactly from its seed.
//
// What runs. Each seed builds a world: a dettest.Clock, seeded random streams split from the seed, the
// durabletest store (the S3 stand-in, on the clock, with per-call faults), the ownertest placement authority,
// an owner FileStore directory, and one agent process ("incarnation") wired as cmd/agentd wires durable mode
// (through internal/agentglue, the glue cmd/agentd runs). Clients, one per database, send writes, failing writes and reads
// through the front pipeline and retry like the real client: same request id, backoff on the clock. The
// agent's own maintenance loop writes back and passivates on its ticker. The sim injects S3 faults (stalls,
// lost replies, failed calls, 503 storms), lost client replies and crashes.
//
// Determinism. Everything runs inside a testing/synctest bubble. The loop lets every goroutine settle
// (synctest.Wait), then either fires exactly one clock timer (dettest.Clock.Step) or runs one sim event
// (crash, restart, storm). Only one thing becomes runnable per step, so the order of effects is the order of
// timers, not of the Go scheduler. Each database has its own client and fault streams, so the few places
// where one event wakes several goroutines (a crash) cannot change another database's draws. Trace lines a
// step produces are sorted by stream before they join the trace; Result.Hash is the trace's SHA-256 and the
// determinism test runs each seed twice and compares it.
//
// Crashes. A crash kills the incarnation at a quiescent point: from then on its store, placement and
// local-state calls fail without effect (gate.go), its contexts are cancelled, and its host is torn down. A
// restart builds a new incarnation on a fresh cache directory (vactor.NewHost empties its cache anyway: a
// durable-mode agent starts from S3) and the same owner directory, whose records are written with
// fsx.WriteAtomic (fsync, rename, fsync dir) before Put returns, so at a quiescent crash point the directory is
// exactly its last-fsync state. Some crashes also leave a torn temp file there, as a crash inside a Put would.
//
// The checker (model.go) holds the client-side history per database and checks after every step, at every
// crash and at the end: nothing is applied twice; a write every answer said "nothing ran" is never applied;
// every database file the store publishes is a prefix of the history and of every later generation; every
// write acknowledged before a successful write-back survives a crash; no acknowledged write is lost unless no
// fault-free window as long as the RPO fit between its acknowledgement and the crash; generations and epochs
// only rise.
//
// Workloads. Workload is the port: SQL (raw SQL with request ids, today's path) is built in; an IR workload
// (docexec behind /v1/ops) implements the same four methods. See docs/notes/determinism/ for the
// fidelity limits and how to replay a seed.
//
// The cluster simulation (RunCluster) runs the same loop over several agents (each wired through
// internal/agentglue with /v1/ops on), controld's real Controller, Locator and ticket issuer (ticket.Rotate over
// tickettest.MemKeys), the real edge, a linearizable placement fake and an in-memory registry, and simulated SDK
// clients (several per database) running register and bank-transfer IR workloads; every call between nodes
// crosses a simulated network (cluster_net.go: seeded delay, loss, duplication, partitions). The nemesis
// creates and moves databases, kills and restarts agents, attests dead VMs and restores their databases,
// partitions nodes, drops the locator's cache, takes Postgres and S3 away. The checker (cluster_model.go)
// adds split brain (one VM per epoch, epochs rising in commit order), loss across a graceful move, loss at a
// kill -9 beyond the RPO, and per-database linearizability (internal/linearize). Requests reaching an actor
// pass a door (doors, cluster_agent.go) that the loop opens one per step, so requests woken together reach
// the actor in arrival order. ShrinkCluster cuts a failing run down to a small op script; Vector
// files under testdata/cluster replay fixed failures.
//
// Run must be called inside a synctest bubble (synctest.Test). Error module: "sim" (it returns violations, not
// wrapped errors).
package sim
