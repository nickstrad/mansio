# Books applied to ts-db-go

Each book has a guide (rules to follow from now on) and, for the newer ones, a plan (refactor items with
evidence). The one-page summary of all of them is [../coding-style.md](../coding-style.md); read that first.
Book notes (per-chapter summaries and raw text) live outside the repo in `~/AI/books/<book>/`.

| Book | Guide | Plan | Book notes |
|---|---|---|---|
| *Effective Software Testing* (Aniche) | [knowledge/testing/README.md](../../knowledge/testing/README.md), `~/AI/writeups/go-testing.md` | adopted already | `~/AI/books/effective-software-testing/`, `~/AI/writeups/aniche_effective_software_testing.md` |
| *Concurrency in Go* (Cox-Buday), ch. 4–5 | [knowledge/concurrency-in-go/README.md](../../knowledge/concurrency-in-go/README.md) | adopted already | `~/AI/writeups/cox_buday_concurrency_in_go_errors_logging.md` |
| *Release It!* (Nygard, 1st ed.) | [release-it/guide.md](release-it/guide.md) — integration points & stability | [release-it/plan.md](release-it/plan.md) (RI-01…16) | `~/AI/books/release-it/` |
| *The Art of PostgreSQL* (Fontaine) | [art-of-postgresql/guide.md](art-of-postgresql/guide.md) — SQL style | [art-of-postgresql/plan.md](art-of-postgresql/plan.md) (PG-01…11) | `~/AI/books/art-of-postgresql/` |
| *Object Storage as Infrastructure* | [object-storage/guide.md](object-storage/guide.md) — S3 patterns & rules | [object-storage/plan.md](object-storage/plan.md) (OS-01…13) | `~/AI/books/object-storage-as-infrastructure/` |

Adding a book: create `docs/books/<book>/{plan.md,guide.md}`, add a row here, and add a short section to
`../coding-style.md` that links to the guide.
