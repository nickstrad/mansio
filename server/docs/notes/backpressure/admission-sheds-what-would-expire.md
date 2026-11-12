# Admission sheds what would expire in its queue

How agentd's admission refuses a request at once when its estimated finish time is past its deadline.

Code: `internal/front/admission.go` (`Acquire`), `internal/front/limiter.go` (`lateness`, `done`). Tests:
`TestLimiterLateness` (the estimate, table), `TestAdmissionShedsWhatWouldExpire` (on and off points),
`TestOverloadShedsInsteadOfExpiring` (the overload scenario).

A request that finds its database, tenant or VM at the in-flight cap used to queue until its deadline and
answer `expired` (504, no `Retry-After`). The queue caps are fixed numbers unrelated to the deadline: 256
queued over 64 in flight is about 4 s of work for 1 s statements, twice the default 2 s deadline.

Now `Acquire` estimates, for each level at its cap, when the request would finish:

    wait   = (queued ahead + 1) / completions per second       (the existing shed Retry-After formula)
    finish = wait + in flight / completions per second          (Little's law for the service time)

from that level's completions in the last whole second (per database, per tenant, VM-wide; the slowest level
decides). If `finish` is past the request's deadline it is shed at once: `shed` 503 with `wait` as
`Retry-After`, counted as `mansio_queue_rejected_total{queue="admission",reason="would_expire"}`
(`AdmissionStats.WouldExpire`; `mansio_admission_shed_total` still counts only queue-full sheds).

The rule behind it: a request that cannot finish before its deadline is cheaper to refuse now than to queue.

Fallbacks, all to the old behaviour (queue until the deadline; the fixed caps stay the hard bound): the
request has no deadline; no level at its cap has a measured rate (the first second of an overload, or a
database that was idle: a database's and tenant's rate lives only while it holds or queues something).

Measured (one database, 2 slots, 50 ms per request = 40/s; offered 200/s with 300 ms deadlines; steady state
of 1.3 s after a 1.2 s warm-up; measured 2026-11-10 on the dev VM):

|                    | served | expired (504 after the whole deadline) | shed at admission           | deepest queue |
| ------------------ | ------ | -------------------------------------- | --------------------------- | ------------- |
| before (`2429639`) | 52     | 203                                    | 0                           | 61            |
| after              | 55     | 0                                      | 203 (slowest refusal 71 µs) | 9             |

The estimate is noisy when the rate is small or changing: a stall in the last second makes the next second
shed more than it must (the refusals are retryable and carry a hint), and a burst from idle is not judged
until a second of completions exists.
