# sqlbench architecture (C4, text)

C4 views of the sqlbench proof of concept as built (see [STATE.md](../STATE.md) and
[README.md](../README.md)). The coldload side is out of scope; it only imports `internal/actor`.
Sources are in `diagrams/`; regenerate with the draw-visual skill's `render.py --update`.

## Level 1: System context

<!-- draw-visual: diagrams/c4-1-context.mmd -->
```text
┌────────────┐
│  Engineer  │
└──────┬─────┘
       │
       1
       ▼
┌────────────┐
│  sqlbench  │
└──────┬─────┘
       │
       ├────────────────┐
       │                │
       2                3
       │                │
       ▼                ▼
┌────────────┐    ┌───────────┐
│SQLite files│    │results CSV│
└────────────┘    └─────┬─────┘
                        │
┌────────────┐          │
│ DuckDB CLI │◄───4─────┘
└──────┬─────┘
       │
       5
       ▼
┌────────────┐
│   report   │
└────────────┘
```

1. The engineer runs `scripts/run.sh <config>` (or `bin/sqlbench`).
2. sqlbench creates, seeds and queries one SQLite file per database.
3. Every attempt goes to `requests.csv`, and every sample tick to `samples.csv` (plus `phases.csv`, `schemas.csv`, `run.json`).
4. The DuckDB CLI reads those files offline. DuckDB is not a runtime dependency.
5. `scripts/report.sh` and `scripts/compare.sh` print the report.

## Level 2: Containers

<!-- draw-visual: diagrams/c4-2-container.mmd -->
```text
┌────────────────┐
│ configs/*.json │
└────────┬───────┘
         │
         1
         ▼
┌────────────────┐
│sqlbench process│
└────────┬───────┘
         │
         ├────────────────────┬───────────────────┐
         │                    │                   │
         2                    3                   5
         │                    │                   │
         ▼                    ▼                   ▼
┌────────────────┐    ┌───────────────┐    ┌────────────┐
│SQLite DB files │    │results/run CSV│    │sqlbench.log│
└────────────────┘    └───────┬───────┘    └────────────┘
                              │
┌────────────────┐            │
│ queries/*.sql  │◄────4──────┘
└────────────────┘
```

1. JSON config: phases, rate shape, mix, clients, actor and schema settings.
2. One Go process holds every database actor. No lock is shared between databases.
3. `results/<run>/`, written by a non-blocking background writer.
4. `queries/report.sql` covers one run and `queries/compare.sql` covers all runs.
5. `results/<run>/sqlbench.log`: the JSON log. An error shown to the user carries a log ID that points to its details (chain and stack) here.

## Level 3: Components (Go packages in the sqlbench process)

<!-- draw-visual: diagrams/c4-3-component.mmd -->
```text
┌────────────┐
│cmd/sqlbench├───2─Run───┐
└──────┬─────┘           │
       │                 │
   1 config              │
       ▼                 ▼
┌────────────┐        ┌─────┐
│    load    │◄──3────┤bench│
└────────────┘        └─────┘
```

1. `cmd/sqlbench` reads the config through `load` (config, phases, rate shapes).
2. It then calls `bench.Run`, with a context that Ctrl-C cancels.
3. `bench` drives the load with `load.Pacer` (how many requests are due), `load.Picker` (which client, database and statement) and `load.Generators`.

`bench` plans and runs the phases: an open-loop schedule feeding a worker pool. It uses the remaining packages.

<!-- draw-visual: diagrams/c4-3-component-bench.mmd -->
```text
┌──────┐
│bench │
└───┬──┘
    │
    ├──────────┬─────────┬────────┬────────┬─────────┬──────────┐
    │          │         │        │        │         │          │
    4          5         6        7        8         9         10
    │          │         │        │        │         │          │
    ▼          ▼         ▼        ▼        ▼         ▼          ▼
┌──────┐  ┌────────┐  ┌─────┐  ┌─────┐  ┌─────┐  ┌───────┐  ┌──────┐
│schema│  │workload│  │retry│  │actor│  │stats│  │outcome│  │panicx│
└──────┘  └────────┘  └─────┘  └─────┘  └─────┘  └───────┘  └──────┘
```

4. `schema`: a reproducible random schema and seed data per database.
5. `workload`: SQL per workload class (reads, writes, `slow_cte`, `cross_join`).
6. `retry`: per-client backoff, jitter, budget, and total timeout.
7. `actor`: one goroutine per database. This is the design under test.
8. `stats`: the CSV writer and resource sampler.
9. `outcome`: the single list of outcome codes (`ok`, `shed`, `gen_full`, ...). `retry` and `stats` use it too.
10. `panicx`: carries a panic from the goroutine that panicked to the one that re-raises it. `actor` uses it too.

## Level 4: Inside one run (bench)

<!-- draw-visual: diagrams/c4-5-run-loop.mmd -->
```text
┌─────────────────┐
│    dispatcher   │
└────────┬────────┘
         │
         1
         ▼
┌─────────────────┐
│    jobs queue   │◄─┐
└────────┬────────┘  │
         │           │
         2           │
         ▼           │
┌─────────────────┐  │
│    N workers    ├──┼──4─────┐
└────────┬────────┘  │        │
         │           │        │
         3           │        │
         ▼           │        ▼
┌─────────────────┐  │ ┌─────────────┐
│actor Submit/Wait│  7 │RequestWriter│
└────────┬────────┘  │ └─────────────┘
         │           │
         5           │
         ▼           │
┌─────────────────┐  │
│   retry.Decide  │  │
└────────┬────────┘  │
         │           │
         6           │
         ▼           │
┌─────────────────┐  │
│  backoff timer  ├──┘
└─────────────────┘
```

1. The dispatcher wakes on 1 ms ticks. `load.Pacer` says how many requests are due and `load.Picker` picks each one. The enqueue never blocks; when the queue is full, the request is recorded as `gen_full` and not sent.
2. N workers take jobs one at a time.
3. A worker calls `Submit` on the database's actor and `Wait`s for the reply (see the next section).
4. Each attempt goes to the `stats.RequestWriter`, which writes asynchronously. If it falls behind, records are dropped and counted rather than stalling the load.
5. With the result, the worker asks the client's `retry.Decide` and retry `Budget` whether to try again.
6. A retry with a delay waits on a timer. A delay of 0 skips the timer and goes straight back to the queue.
7. When the timer fires, the retry is enqueued. If the queue is full, the retry is recorded as lost.

An `outstanding` counter covers every job that is queued, running or waiting out a backoff; the run ends when it drains to 0. On Ctrl-C, no new retries start and pending backoffs are cancelled and recorded as never sent.

## Level 4: Inside one actor

<!-- draw-visual: diagrams/c4-4-actor.mmd -->
```text
┌────────────┐
│bench worker│
└──────┬─────┘
       │
   1 Submit
       ▼
┌────────────┐
│  mailbox   │
└──────┬─────┘
       │
       2
       ▼
┌────────────┐
│ actor loop │
└──────┬─────┘
       │
       ├───────────────┐
       │               │
   3 writes         4 reads
       │               │
       ▼               ▼
┌────────────┐    ┌─────────┐
│ write conn │    │read pool│
└──────┬─────┘    └────┬────┘
       │               │
       5               │
       ▼               │
┌────────────┐         │
│SQLite file │◄───6────┘
└────────────┘
```

1. `Submit` never blocks. When the mailbox is full, the request is shed (`ErrMailboxFull`).
2. The loop drains a batch. A request still queued past its deadline gets `ErrExpired` without running.
3. All writes in the batch run in one `BEGIN IMMEDIATE … COMMIT` (group commit). If one write times out, the batch rolls back (`ErrAborted`).
4. Reads run after the commit, on the write connection when `read_conns = 0`, otherwise on the read pool.
5. The write connection commits to the database file (WAL by default).
6. Pool readers run alongside the writer in WAL mode. The statement timeout interrupts SQLite through the context.
