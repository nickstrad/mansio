# mansio server

The Go half of mansio: the processes that store and serve documents. The TypeScript SDK sends them requests
of the intermediate representation (IR) protocol ([spec](../ir/spec/v1.md)); the
server validates each request, compiles it to SQLite SQL and runs it on the database's own goroutine. No
application code runs here.

Module: `github.com/nickstrad/mansio/server`. Go 1.26 or newer.

## Commands (`cmd/`)

| Command    | What it is                                                                                              |
| ---------- | ------------------------------------------------------------------------------------------------------- |
| `agentd`   | The data plane, one per VM: serves `POST /v1/ops`, one actor (goroutine + SQLite file) per database.     |
| `controld` | The control plane: tenant, database and VM registries, placement in Postgres, the locator, tickets.     |
| `edge`     | A stateless TLS proxy in front of the agents: verifies the ticket and routes by VM.                     |
| `agentctl` | Operator CLI for one agent's admin listener: drain, backup, passivate, limits, breakers, log level.     |
| `dbtool`   | Operator CLI for the durable copies in object storage: inspect, verify, restore, import, gc.            |
| `bench/`   | Load tests, not part of the platform: `sqlbench`, `frontbench`, `wbbench`, `coldload`.                  |

`controld` is also its own admin CLI (`controld create`, `move`, `attest-dead`, ...; see `cmd/controld/cli.go`).
Each daemon reads one JSON config; the reference is its `Config` type (`cmd/<name>/config.go`) and the samples
in `configs/`.

## Packages (`internal/`)

The request path, outermost first:

| Package                        | Role                                                                                    |
| ------------------------------ | --------------------------------------------------------------------------------------- |
| `edge`                         | The edge proxy: ticket check, routing, per-agent bulkheads and breakers.                |
| `front`                        | agentd's request pipeline: credentials, admission limits, the `/v1/ops` handler, codes. |
| `ticket`, `keyfetch`           | Ed25519 access tickets and keeping the verifier's key ring current.                     |
| `tenancy`                      | Tenants, API keys, users and sessions (single-node credential modes).                   |
| `ir`                           | The IR wire model: strict decoding, canonical JSON, limits, error values.               |
| `docschema`, `schemareg`       | The document schema model and fingerprints; the app schema registry client.             |
| `doccompile`                   | Document operations to SQL and rows back to documents (port of `core/compile/`).        |
| `docexec`                      | Executes a decoded request on a database actor: preconditions, writes, versions.        |
| `actor`                        | One goroutine per database: mailbox, group commit, read pool, request-id records.       |
| `vactor`, `durable`, `objstore` | Durable mode: databases fetched from and written back to S3 as immutable generations.   |
| `owner`, `placement`           | Who may serve a database: epochs, fencing, moves; file and Postgres authorities.        |
| `control`                      | The control plane's lifecycle controller, placement policy, locator and agent client.   |

Supporting packages: `eventlog` (the request log: its record format, queue, files and limits;
[docs/observability.md](../docs/observability.md)), `metrics`, `capacity` (generates
[docs/capacity.md](docs/capacity.md)), `breaker`, `slots`, `ratelimit`, `retry`, `client` (the Go client),
`logx`, `errs`, `assert`, `tlsx`, `fsx`, `det` (clock, randomness and id seams for deterministic tests) and
`sim` (the deterministic cluster simulator). `bench`, `load`, `workload`, `schema`, `stats`, `outcome` and
`coldload` belong to the load tests.

## Build and test

```sh
go build ./...
go test -race ./...            # or, from the repository root: deno task go:ci
make help                      # benchmark, simulator and soak targets
```

The end-to-end suite that drives these binaries through the SDK is in `../tests/e2e/platform/`
(`../scripts/dev/e2e-linux.sh`). Tests that need Postgres or an S3 store are skipped unless their environment
variables are set; [docs/notes/testing/gated-suites.md](docs/notes/testing/gated-suites.md)
says how to run them.

## Other folders

| Folder     | Contents                                                                                          |
| ---------- | ------------------------------------------------------------------------------------------------- |
| `configs/` | Sample configs for each daemon and the load tests.                                                |
| `deploy/`  | Compose files for Postgres and SeaweedFS, and the Prometheus alert rules.                         |
| `scripts/` | Benchmark, soak and simulator drivers.                                                            |
| `queries/` | DuckDB SQL for reading benchmark results.                                                         |
| `docs/`    | [Coding style](docs/coding-style.md), how mansio uses [object storage](docs/object-storage.md) and [Postgres](docs/postgres.md), the [capacity table](docs/capacity.md), the control-plane plan, C4 notes, and the design [notes](docs/notes/README.md). |
