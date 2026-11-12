# Leases: options to bound a partitioned zombie's stale reads

The options for bounding how long a partitioned but live old owner keeps serving after a restore, and the one
recommended. Not decided; nothing here is built.

## The gap

Safety today has no clocks ([safety argument](safety-argument.md)). When an operator restores a database away
from a VM that is partitioned but alive (wrongly attested dead), the old owner (a zombie) keeps admitting at
epoch E until its next publish fails against the new owner's stamp (`ErrSuperseded`), then fences itself.
Until then:

- **Writes** it acknowledges after the new owner's stamp are lost (never published). Bounded by its next
  write-back: `Policy.WriteBackCommits` / `WriteBackEvery`, or never if the zombie only reads.
- **Reads** it serves are stale: they miss the new owner's writes. Not bounded at all for a read-only or idle
  database: no publish happens, so nothing stops it.

Clients reach the zombie only through a stale route (cached ticket/route to the old VM); tickets expire
(front, authentication only), which bounds how long a _given client_ can keep using a stale route, but not the
zombie itself.

## Options

|                               | What                                                                                                                                                                               | Bounds                                                       | Cost / risk                                                                                                                                                                                                                                                          |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A. Status quo                 | No leases; document the gap; restores only on attested death                                                                                                                       | Writes: next publish. Reads: unbounded                       | Zero code. Relies fully on the operator's attestation                                                                                                                                                                                                                |
| B. Owner lease (self-fencing) | Owner renews `lease_until` in placement every T/3; stops admitting (FENCING, no write-back) when `now > lease_until − drift`. Restore of a non-responding VM waits `T + max drift` | Reads and writes: T + drift after the partition              | Liveness depends on Postgres: a placement outage longer than T stops every database on every VM (needs a degraded-mode decision like the WAL one). Clocks enter _availability_, not safety, if the restore wait is honored; if an operator skips the wait, back to A |
| C. Read stamp check           | Before serving a read, at most every R seconds per key, the owner re-reads the manifest (1 GET) and fences itself if the epoch moved                                               | Reads: R + one GET latency, only after the new owner stamped | +1 S3 GET per key per R on read paths (at R = 1 s, 1000 hot keys ≈ 86 M GET/day ≈ $35/day at list price). No clocks for safety. S3 outage → reads must either fail (unavailable) or skip the check                                                                   |
| D. Ticket-bound               | Shorten ticket lifetime and make controld stop issuing tickets for the old VM on restore                                                                                           | A client's stale route: ticket TTL                           | Does not stop the zombie; a client holding a fresh ticket from before the restore still reaches it. Cheap; complements B or C                                                                                                                                        |

## Recommendation

**B (owner lease) with a long T (e.g. 30 s), plus D**, once the placement-outage behavior is decided. Reasons:

- It is the only option that bounds both stale reads and lost writes without a per-request cost, and the bound
  (T + drift) is what an operator can reason about during a restore.
- Safety stays clock-free: the epoch + stamp backstop is unchanged; the lease only shortens the window, and a
  restore that ignores the wait degrades to today's behavior, not to a fork.
- T long (30 s) keeps renewals cheap (one UPDATE per VM per 10 s if renewed per VM, not per key) and tolerates
  a short Postgres failover; restores are rare, manual events where waiting 30 s + drift is acceptable.

Renew per VM, not per key: one row per VM (`vm_lease_until`) that `Claim`/admission consult locally, so the
cost is independent of the number of databases.

**Open question (blocks B), not decided:** when placement is unreachable for longer than T, should a VM stop
serving (safe, but a Postgres outage becomes a full outage) or keep serving at the risk of a longer zombie
window? This is the same shape as the open WAL degraded-mode question (refuse writes under an open breaker, or
ack locally with a larger RPO) and should be decided with it. Until then: A + D, and keep "restore only on
attested death" in the runbook.
