# coldload architecture (C4, text)

These are C4 views of the coldload work as built: it takes SQLite files stored in SeaweedFS, turns them into
ready virtual actors, and measures how long that takes. See
[STATE-coldload.md](../../agent-work/server/state_files/STATE-coldload.md) (archived) and
[docs/notes/coldload/](notes/coldload/README.md). Running actors under request load belongs to
sqlbench ([architecture-c4.md](architecture-c4.md)). Sources are in `diagrams/coldload-c4-*.mmd`;
regenerate them with the draw-visual skill's `render.py --update`.

## Level 1: System context

<!-- draw-visual: diagrams/coldload-c4-1-context.mmd -->
```text
┌────────────┐
│  Engineer  │
└──────┬─────┘
       │
       1
       ▼
┌────────────┐
│  coldload  │
└──────┬─────┘
       │
       ├────────────────┐
       │                │
       2                3
       │                │
       ▼                ▼
┌────────────┐    ┌───────────┐
│SeaweedFS S3│    │results CSV│
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

1. The engineer runs `make coldload-bench CONFIG=sizes` (or `scripts/coldload/run.sh`, or `bin/coldload`).
2. `seed` uploads copies of a template SQLite file per size class. `run` downloads them again (S3 API, SeaweedFS in Docker).
3. Each run writes CSV files and a JSON log.
4. The DuckDB CLI reads the CSVs offline.
5. `scripts/coldload/report.sh` prints the report.

## Level 2: Containers

<!-- draw-visual: diagrams/coldload-c4-2-container.mmd -->
```text
┌────────────────┐
│scripts/coldload│
└────────┬───────┘
         │
         1
         ▼
┌────────────────┐
│coldload process│
└────────┬───────┘
         │
         ├─────────────────┬──────────────────┐
         │                 │                  │
         2                 3                  4
         │                 │                  │
         ▼                 ▼                  ▼
┌────────────────┐    ┌─────────┐    ┌────────────────┐
│SeaweedFS :8333 │    │cache dir│    │results/coldload│
└────────────────┘    └─────────┘    └────────┬───────┘
                                              │
┌────────────────┐                            │
│   report.sql   │◄5──────────────────────────┘
└────────────────┘
```

1. `run.sh` starts SeaweedFS if needed, takes the shared bench lock `results/.bench-lock`, and refuses to seed
   with less than 20 GB free on the host or less than 12 GB in the Docker VM. It runs `cleanup.sh local` on every exit.
2. `coldload-seaweedfs` container: S3 on `:8333` with the data in the `seaweed-data` volume. `cleanup.sh all` removes it.
3. Local cache dir (default `<out_dir>/cache`): fetched `.sqlite` copies. The host empties it on start and
   removes it on Close.
4. `results/coldload/<run>/` holds `activations.csv`, `levels.csv`, `run.json` and `coldload.log`.
5. `queries/coldload/report.sql`, run by DuckDB.

## Level 3: Components (inside the coldload process)

<!-- draw-visual: diagrams/coldload-c4-3-component.mmd -->
```text
┌────────────┐
│cmd/bench/coldload│
└──────┬─────┘
       │
       ├────────────────┐
       │                │
       1                2
       │                │
       ▼                ▼
┌────────────┐    ┌───────────┐
│  coldload  │    │  recorder │
└──────┬─────┘    └───────────┘
       │
       3
       ▼
┌────────────┐
│vactor.Host │
└──────┬─────┘
       │
       ├────────────────┐
       │                │
       4                5
       │                │
       ▼                ▼
┌────────────┐    ┌───────────┐
│ s3Fetcher  │    │actorOpener│
└──────┬─────┘    └─────┬─────┘
       │                │
       6                7
       ▼                ▼
┌────────────┐    ┌───────────┐
│  objstore  │    │   actor   │
└────────────┘    └───────────┘
```

1. `cmd/bench/coldload` (`realMain` and its reporter, `seed`, `run`) uses the `internal/coldload` domain package (no I/O). That
   package holds `Config.Normalize`, the `Levels` plan, `RunLevel` and the CSV rows.
2. `recorder` is an async CSV writer for `activations.csv`. It drops and counts rows instead of blocking.
3. `RunLevel` calls the host through its `Getter` port. `vactor.Host` provides `Get` and `Passivate` and
   uses singleflight, so concurrent requests for one key share a single activation.
4. The `Fetcher` port is implemented by `s3Fetcher`. The `s3Fetcher` and `actorOpener` adapters live in `cmd/bench/coldload`.
5. The `Opener` port is implemented by `actorOpener`.
6. `internal/objstore` is the S3 client (minio-go, one GET per fetch) and talks to SeaweedFS.
7. `internal/actor` (owned by sqlbench) runs one goroutine per database on the SQLite file.

The shared packages `internal/errs`, `internal/logx` and `internal/assert` are used throughout and are not drawn.
Tests swap the ports for fakes in `internal/vactor/vactortest`.

## Level 4: One activation

<!-- draw-visual: diagrams/coldload-c4-4-activation.mmd -->
```text
┌─────────┐     ┌─────────┐     ┌──────┐     ┌────┐     ┌───────┐
│ worker1 │     │ worker2 │     │ Host │     │ S3 │     │ actor │
└────┬────┘     └────┬────┘     └───┬──┘     └──┬─┘     └───┬───┘
     │               │              │           │           │
     │ 1. Get key    │              │           │           │
     ├─────────────────────────────►│           │           │
     │               │              │           │           │
     │               │ 2. Get key   │           │           │
     │               ├─────────────►│           │           │
     │               │              │           │           │
     │               │      ┌────────────────┐  │           │
     │               │      │ one activation │  │           │
     │               │      └────────────────┘  │           │
     │               │              │           │           │
     │               │              │ 3. GET    │           │
     │               │              ├──────────►│           │
     │               │              │           │           │
     │               │              │ 4. Start  │           │
     │               │              ├──────────────────────►│
     │               │              │           │           │
     │               │              │ 5. first query        │
     │               │              ├──────────────────────►│
     │               │              │           │           │
     │               │              │ 6. warm   │           │
     │               │              ├──────────────────────►│
     │               │              │           │           │
     │ 7. Result     │              │           │           │
     │◄┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┈┤           │           │
     │               │              │           │           │
     │               │ 8. Result, waited        │           │
     │               │◄┈┈┈┈┈┈┈┈┈┈┈┈┈┤           │           │
     │               │              │           │           │
```

1. A `RunLevel` worker calls `Host.Get(key)` for a cold key. The host starts the activation on its own
   goroutine, so one caller's cancel does not fail the others.
2. A second caller for the same key waits on that same activation (singleflight). In the `sizes` run, 500
   callers led to one fetch.
3. `objstore.GetToFile` makes one S3 GET into `<key>.part` and then renames it.
4. `actorOpener.Open` runs `actor.New(...).Start(true)`.
5. The first query is sent through `actor.Do`.
6. Optional warm-up queries run next.
7. The starting caller gets `Result` with its `Activation` timings. The worker then records it, and the
   recorder channel writes it to `activations.csv`.
8. The waiter gets the same database with `Waited` set.

A failed activation is forgotten and its local file removed, so the next `Get` retries. A panic in the
activation is re-raised in every waiting caller.
