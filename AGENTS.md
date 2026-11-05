# AGENTS.md — ts-db-go

Instructions for every agent working in this repo.

## Before planning or writing code

1. Read [docs/coding-style.md](docs/coding-style.md): the one-page rules for structure, contracts, errors,
   concurrency, integration points, object storage, SQL and testing.
2. Open the full guide for the area you touch (all listed in [docs/books/README.md](docs/books/README.md)):
   - S3 / `objstore` / `durable` / `vactor` storage paths → [docs/books/object-storage/guide.md](docs/books/object-storage/guide.md)
   - any remote call, queue, timeout, retry, health or lifecycle code → [docs/books/release-it/guide.md](docs/books/release-it/guide.md)
   - any SQL (SQLite, DuckDB `queries/`, Postgres placement) → [docs/books/art-of-postgresql/guide.md](docs/books/art-of-postgresql/guide.md)
   - tests → [knowledge/testing/README.md](knowledge/testing/README.md); errors/goroutines → [knowledge/concurrency-in-go/README.md](knowledge/concurrency-in-go/README.md)
3. When planning, check the book plans (`docs/books/*/plan.md`) for open items in the code you will touch, and
   cite the guide rules (e.g. `RI T1`, `OS W2`, `SQL G-8`) your plan relies on.
4. Read `STATE.md`, `shared_state.md` and [knowledge/AGENTS.md](knowledge/AGENTS.md) (shared notes rules and
   multi-agent coordination).

## When done

Run `gofmt -l .`, `go vet ./...`, `go test -race ./...`, `go test -tags noassert ./...`. If you added an
integration point, a storage layout, or a SQL file, update the matching guide and its tests.
