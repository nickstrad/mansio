# Learning path: platform operator

For someone who runs the platform: starts and configures the processes, creates and moves databases, watches
load, and recovers from failures. You need to read configs and a little Go for reference, but not the SDK
internals. Work through this top to bottom. Each step is something to run, read or try, followed by why it
matters. Paths are from the repo root. Setup for every command: `export PATH="$HOME/.deno/bin:$PATH"`.

`make platform-up` needs Linux: `scripts/platform/lib.sh` uses GNU `realpath -m`, `sha256sum`, `setsid` and
`/proc`, and on macOS it stops at once with `realpath: illegal option -- m`. Run it on a Linux machine or VM.
Every drill from Phase 3 on uses that cluster.

## Phase 1: Know the processes

1. Open <https://nickstrad.github.io/mansio/> and read the `context` and `containers` views.

   These are the things you run: controld, the edge, agentd (one per VM), Postgres for placement, and S3. The
   caller's process with the SDK is not yours. Keep the tab open as your map.

2. Read the list under "The Go binaries:" in `README.md` (in "Repository layout").

   Four binaries (`agentd`, `controld`, `edge`, `dbtool`) and agentd's two modes: static (local files, no
   moves) and durable (S3 generations, placement, moves). Almost every operator question starts with "which
   mode?".

3. Run `make platform-up` and read every line it prints; leave it running for the next phases.

   This starts the whole multi-node shape on loopback (controld, two agentd, the edge, Postgres, SeaweedFS)
   and proves it with an SDK write and read. It is your practice cluster for the rest of this guide.

4. Read `scripts/platform/up.sh` top to bottom, and look inside `.platform/`.

   The script is a complete, commented deployment recipe: the CA and certificates, the ticket key ring, each
   secrets file, each config, the start order and the readiness checks. `.platform/` holds what it generated,
   including each process's config and log.

5. Follow `docs/guide.md` "Configure and start agentd" once by hand.

   A single static agentd is the smallest deployment. Writing its config and secrets file yourself teaches the
   rules that apply everywhere: unknown fields are errors, secrets files must be mode 0600, and `-check`
   validates a config without starting (it does not check the secrets file's mode; starting does).

## Phase 2: Configuration and credentials

6. Read the `Config` types: `server/cmd/agentd/config.go`, `server/cmd/controld/config.go` and
   `server/cmd/edge/config.go`, beside the samples in `server/configs/`.

   There is no separate config manual: the field comments are the reference. Compare each sample with the file
   `platform-up` generated for the same process.

7. Read "Authentication and tenancy" in `docs/guide.md`, then build `server/cmd/mansioctl` and run
   `mansioctl -h` to see its commands.

   Single-node agentd can authenticate with app tokens, tenant API keys or sessions; multi-node deployments
   authenticate at controld. This decides where tenants and keys live and which tool manages them.

8. Read the comment at the top of `server/cmd/controld/keygen.go`, then look at
   `.platform/controld/ticket-keys/` and `.platform/ticket-keys.public.json`.

   Tickets are how agents trust a request without calling controld: controld signs them and agents verify them
   offline with the public keys. Losing or mixing key rings is a full outage, so know where they live.

9. Read `server/docs/notes/front/2026-11-05-controld.md`.

   It describes what controld owns: the VM registry, database placement, the locator, and the lifecycle flows
   with what to do when each one stops halfway. It is the control plane's operating manual.

## Phase 3: Day-to-day operations (practise on the platform-up cluster)

10. Run controld's admin CLI:
    `.platform/bin/controld vms -addr https://127.0.0.1:27080 -secrets .platform/controld/secrets.json -ca .platform/tls/ca.pem`,
    then read the command table in `server/cmd/controld/cli.go`.

    Every admin action is one scriptable command over the admin API: `create`, `get`, `move`, `suspend`,
    `resume`, `drop`, `attest-dead`, `restore`, `create-key`, `revoke-key`, `limit`, `vms`. If the flags
    differ, the command prints its own usage. Every command needs the same `-addr`, `-secrets` and `-ca`
    flags; the later steps leave them out for brevity.

11. Create a database and an API key with `create -tenant <tenant id> -name drill1` and
    `create-key -tenant <tenant id> -principal <you>`, then `get -db "<tenant id>:drill1"`. The tenant id is a
    UUID: list tenants with
    `mansioctl -addr https://127.0.0.1:27080 -ca .platform/tls/ca.pem -token-file .platform/controld/secrets.json -format table tenants list`.
    Then read the placement row in Postgres:
    `docker exec <postgres container> psql -U mansio -d mansio -c 'select database_id,owner_vm,epoch,generation,state from control.placement'`.

    `get` shows only the registry state (active, moving, suspended, dropped). The placement row shows which VM
    owns the database, at which epoch and generation: the three values every lifecycle operation changes.
    Always quote a database key in the shell; in zsh an unquoted `$T:name` is silently mangled.

12. Move the database with `move -db <key> -to <other vm>`, read the placement row again, and open the
    `database-move` view on the diagram site.

    A move is mark moving, fence on the source (with a final write-back), epoch flip in Postgres, claim on the
    target. The epoch goes up by one, and clients re-resolve on their own. This is the operation you will run
    most for rebalancing and maintenance.

13. Try `suspend`, `resume` and `drop` on a throwaway database.

    These complete the lifecycle. Note that `drop` deletes the database's objects in S3, so it is the one
    command here that cannot be undone, and a dropped name cannot be created again.

14. Run `go run ./cmd/agentctl` from `server/` to print its usage, then try `backup <key>`, `passivate <key>`
    and `loglevel debug` against the admin listener of the agent that owns the database, for example
    `agentctl -addr 127.0.0.1:27072 backup "<key>"` (vm-a is 27072, vm-b is 27075; `-addr` goes before the
    command).

    `agentctl` is the per-agent control: force a write-back, drop a warm database from local disk, trip or
    reset a breaker, change a limit, drain before a restart. The admin listener is loopback only, which is its
    only access control.

15. Drain and restart one agent: `agentctl -addr 127.0.0.1:27075 drain`, watch
    `curl http://127.0.0.1:27074/readyz` turn 503 "draining", stop it with
    `kill -TERM $(cat .platform/vm-b.pid)`, and start it again with
    `cd .platform/vm-b && ../bin/agentd -dev -config agentd.json`.

    This is the planned-maintenance procedure. A clean shutdown writes every dirty database back first, which
    is why a planned restart loses nothing and a crash can.

## Phase 4: Durability, backups and restore

16. Read "Backups and restore" in `docs/guide.md`, especially the paragraph in bold.

    It states the promise exactly: the acknowledgement is the local commit, write-back to S3 is asynchronous,
    and a host loss can lose about 11 seconds of acknowledged writes with the defaults, or the whole outage
    while S3 is unreachable. You must know this number before you promise anything to a customer.

17. Read `server/docs/notes/writeback/2026-11-06-backup-paused-open-issues.md`.

    The S3 work is built but paused pending a decision on LiteFS. This note lists what is and is not
    guaranteed today, so you know which gaps are known and which would be new bugs.

18. Read `server/docs/notes/writeback/2026-11-05-storage-runbook.md`, then run `dbtool` without a command for
    its usage.

    This is the storage runbook: `inspect`, `verify`, `orphans`, `gc`, `rebuild-manifest`, `restore` and
    `import`. `restore` never overwrites a file, and `verify` checks size, checksum and SQLite integrity.

19. Back up a database with `agentctl backup "<key>"`, then restore that generation to a new file and open it
    with `sqlite3`:
    `DBTOOL_S3_ACCESS_KEY=mansiodevkey DBTOOL_S3_SECRET_KEY=mansiodevsecret dbtool -endpoint 127.0.0.1:18333 -bucket mansio-test -root platform/ restore -gen <G> "<key>" <new file>`
    (build it with `cd server && go build -o dbtool ./cmd/dbtool`; the new file must not exist yet).

    A backup you have never restored is not a backup. Doing it once on the practice cluster makes the real one
    routine.

20. Practise losing a VM: kill one agentd with `kill -9`, then run `attest-dead -vm <vm> -by <you>` and
    `restore -vm <vm>`, and `get` a database it owned.

    When a VM is gone for good, the operator attests it dead and controld moves its databases to live agents
    from their last published generations. Attesting accepts the loss of that VM's unpublished writes, which
    is why a person must do it.

## Phase 5: Watching it

21. Run `curl -s http://127.0.0.1:27071/metrics | grep mansio_` (vm-a's health listener in the practice
    cluster), the same on `27444` for the edge, and
    `curl -s --cacert .platform/tls/ca.pem https://127.0.0.1:27080/metrics | grep mansio_` for controld.

    agentd and the edge serve `/healthz`, `/livez`, `/readyz` and `/metrics` on a health listener; controld
    serves `/livez`, `/readyz` and `/metrics` on its HTTPS API listener. Readiness is what a load balancer
    should use; `/metrics` is Prometheus text.

22. Read `server/docs/capacity.md`: the three tables first (agentd, controld, edge), then two of the runbooks.

    The tables list every bound on a request's path with its config key, default, metric and runtime lever
    (alerts are in the "Alerts" section), and each row has a runbook. When something refuses or queues, this
    page tells you which limit it is and which lever to pull.

23. Read `server/deploy/prometheus/backpressure.rules.yml`.

    These are the shipped alert rules. The platform does not ship a Prometheus server, so loading these rules
    into yours is part of deploying.

24. Watch `mansio_vactor_oldest_dirty_age_seconds` and `mansio_vactor_dirty` while you stop the practice
    cluster's SeaweedFS container (`docker stop`) for three minutes while a client keeps writing, then start
    it again.

    This is the durability promise breaking in front of you: writes keep being acknowledged while nothing is
    published, and the dirty age grows from under 10 seconds to the length of the outage, then drops back
    within a minute of recovery. The `MansioUnpublishedWritesOld` alert (older than 60 s for 2 min) exists for
    exactly this.

25. Read `server/docs/notes/backpressure/README.md` and `docs/retry-contract.md`.

    Together they say what overload looks like from both sides: which process refuses, with which code, and
    what clients do next. They let you tell "the platform is protecting itself" from "the platform is broken".

26. Change a limit at run time: `controld limit -queue resolve_load -n 8` (plus the usual flags) and
    `agentctl -addr 127.0.0.1:27072 limit verify 8`.

    Most bounds can be moved without a restart, and controld refuses a value that would break its Postgres
    connection budget. Know this before an incident; the change is not written to the config file.

27. Read "Observability" in `docs/guide.md` and `docs/observability.md`. The practice cluster does not enable
    the event log, so add `"event_log": {"dir": "<absolute dir>"}` to `.platform/vm-a/agentd.json`, restart
    that agent (step 15), make some calls, then run
    `LOG_DIR=<dir> DATA_DIR=<scratch dir> deno task logs:ingest`, `deno task logs:list` and
    `deno task logs:query handler-throughput` with the same variables.

    Metrics tell you that something is wrong; the event log tells you what happened, request by request. The
    writer bounds its own disk use (4 GiB and 45 days by default) and `logs:sweep` trims what has been
    ingested.

28. Make a process fail at startup (for example point `tls.cert_file` at a missing file in a copy of its
    config), then find the printed log ID in its log (`.platform/<process>/*.log`).

    Internal errors and startup failures name a log ID, and the log holds the full record under it. This is
    the first place to look when a process will not start or a call returns `internal`.

## Phase 6: Failure and load drills

29. Read the "Troubleshooting" section of `docs/guide.md`.

    Each entry is symptom, cause, fix, for the errors callers report most: `ticket_invalid`, `wrong_owner`,
    `SchemaDriftError`, `shed`, `expired`. This is your first-line support sheet.

30. Read `server/docs/notes/fencing/2026-11-05-safety-argument.md`.

    It explains why two agents can never both serve one database: epochs in Postgres, fencing on the agent,
    stamps in S3. You do not need the proof, only the rule it gives you: never bypass the epoch by hand.

31. Stop Postgres on the practice cluster, make a few calls, start it again, and watch what kept working.

    A client that is already running keeps serving warm databases on its cached route and ticket (tickets last
    60 seconds here); new clients, new databases and lifecycle operations fail, and `/readyz` stays 200
    throughout. Knowing the blast radius of each dependency is most of incident response.

32. Read `perf/README.md` and follow its manual-run recipe: `deno task perf:smoke` needs k6 and a static
    agentd with tickets, not the practice cluster.

    The k6 scripts are how you measure a deployment before trusting it with load. The numbers in the capacity
    table are defaults, not measurements of your machines.

33. Read `server/docs/control-plane-plan.md` (skim for the decisions) and `agent-work/server/shared_state.md`
    from the bottom up (its newest entry is last).

    The plan records what the control plane promises and what was left out; `shared_state.md` records every
    behaviour change since. Read the new entries after each upgrade.

## Phase 7: Check yourself

34. Without notes, write the steps to replace a VM with zero lost writes, and the steps when the VM is already
    dead.

    The first is drain and move (or clean shutdown); the second is attest-dead and restore, with an accepted
    loss window. If you can say why they differ, you understand durability here.

35. Without notes, answer: callers report 503 `shed`. Which three places can refuse, and how do you tell which
    one did?

    The edge, agentd's admission and controld's resolve gates each refuse, and
    `mansio_queue_rejected_total{queue=...}` names the one. Reread step 22 if unsure.

36. Run `make platform-down`.

    It removes the containers, volumes and `.platform/`. Always start the next practice session from a clean
    `platform-up`.
