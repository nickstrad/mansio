# Architectural decisions

Dated decisions with rationale. Plans carry the _what_; this page carries the _why_. Newest first.

## 2026-10-15 — `docs/pages/` consumes the published c4-sdk npm package

Decision (Nick's standing direction, relayed by Twin; plan item PG-03): once `c4-sdk` 0.1.0 was published to
npm, the interim copy in `docs/pages/vendor/c4-sdk/` (with `sync.sh` and `PROVENANCE.md`) was deleted and the
site now depends on `c4-sdk@0.1.0`, pinned exactly. Why: the copy existed only until the package was
published, and its design (reached only through the package's own specifiers, only from
`docs/pages/src/sdk.ts`) made the swap a dependency change plus the worker factory. The package bundles its
runtime dependencies, so the site dropped `@base-ui/react`, `@xyflow/react`, `elkjs`, `react-markdown` and
`zod`. The package is never patched; the site adapts to its public surface.

## 2026-10-15 — Supporting containers carry the `external` tag in both diagram datasets

Decision (PG-01 D3, applied again in PG-02 P12; landed `f52c817`): in `docs/pages/`, any container that
appears on a component view next to the component being drawn is tagged `external`. Why: c4-sdk rejects a
non-component, non-person member of a component view without that tag, and the site must build. The cost is
that tags are per element, so the tag spreads to the container views: `reader`, `files`, `registry` and
`jsonl` in `mansio-current`, and `api`, `files` and `lane-jsonl` in `mansio-lanes-proposal`, render grey on
every view although they sit inside the platform boundary. Each dataset's container-view keys say so and the
boundary groups show membership. This is wrong for the model (the scope system's own sibling containers are
not external), so the standing c4-sdk hand-off asks upstream to allow sibling containers of the scope's system
on a component view without the tag. When it lands, bump the `c4-sdk` dependency
(`npm install --save-exact c4-sdk@<version>` in `docs/pages/`) and drop the tags from both inventories (D3,
P12) in the same change. Details: `docs/pages/content/*/inventory.md`.

## 2026-10-15 — Architecture diagrams site in `docs/pages/` on an interim c4-sdk copy

Decision (Nick, relayed by Twin on 2026-10-14; plan `docs/plans/pages-diagrams.md` on `plan/c4-react`; built
by PG-01): mansio gets an interactive C4 site in `docs/pages/`, a separate npm project, showing what runs on
`main` next to proposed designs, for use while designing features. Until c4-sdk 0.1.0 is on npm, the site
renders through a copy of the c4-sdk source in `docs/pages/vendor/c4-sdk/`. Why this shape: the swap must be
"delete the directory and add the dependency", so the copy is reached only through the package's own
specifiers (`c4-sdk`, `c4-sdk/core`, `c4-sdk/styles.css`, as Vite aliases and TS paths) and only from
`docs/pages/src/sdk.ts`. `deno.json` is frozen and the root `deno fmt`/`deno lint` scan `docs/pages/`, so the
copy is made compliant by `sync.sh` (fmt plus per-file lint-ignore headers), never by hand, and
`sync.sh --check` proves it matches the recorded upstream commit.

The datasets are converted copies pinned to commits, not a second source of truth: `docs/architecture/` stays
the audited current architecture, and its maintenance protocol is unchanged. A proposed dataset re-checks the
source's "planned" claims against `main` and converts what has landed as `existing`, with a note in its
inventory (`docs/pages/content/<dataset>/inventory.md`). See [docs/pages/README.md](../pages/README.md).

## 2026-10-14 — Statement execution through the FFI binding, not `node:sqlite`

Decision (plan `docs/plans/completed/ffi-implementation.md`, ADR 0008): the API process opens every
application SQLite file through `platform/sqlite_ffi/`, a `Deno.dlopen` binding over the system `libsqlite3`.
`node:sqlite` leaves the server's module graph. Why: a synchronous `DatabaseSync` call cannot be stopped, so a
long `count` held the event loop and the request could only fail by timeout while the statement kept running.
`sqlite3_interrupt` can stop it. The cutover is one commit with no flag, because two copies of SQLite must
never open one file in one process. Unbounded reads run under `STATEMENT_DEADLINE_MS` and fail HTTP 504 when
they pass it. Point reads and writes stay synchronous and are never interrupted.

## 2026-10-13 — Parity gate amended: blocking p50 within 1.3× of `node:sqlite` plus 1 µs

Human decision (nickstrad, ~23:03 CDT, 2026-10-13, relayed through the coordinator). The plan's first read
gate, "blocking p50 ≤ `node:sqlite` p50 in the same run", cannot be met by a per-step native call: four FFI
crossings per read cost approximately 6.0 to 6.6 % over node's single call. The gate is now
`blocking p50 ≤ 1.3 × node p50 + 1 µs`, and it passes on the FFI-06 quiet-host runs. Strict parity is recorded
as not met. The end-to-end HTTP point reads are faster than before the cutover. See ADR 0008 and plan risk 19.

## 2026-10-13 — Fresh agent and worktree for every unit

Human decision: each build, review, repair/fixup, documentation follow-up, and side quest uses a newly spawned
agent with fresh context in its own worktree. Completed sessions are never reused for another unit, with no
convenience exceptions. Before closure, commit the work, assess and commit useful knowledge/scripts through
ownership-safe handoffs, and clean the worktree after coordinator inspection. This keeps durable findings in
Git rather than in a reused conversation. The canonical
[fresh-agent lifecycle](../../agent-work/AGENTS.md#fresh-agent-lifecycle-every-unit-of-work) defines the
sequence; [event reporting](../../agent-work/event-log.md#write-protocol-claude-and-codex) uses a distinct
actor per unit and permits resume only within the same still-active unit.

## 2026-10-11 — Narrowed SQLite pivot: in-process document reads

DuckDB stays responsible for the event log and named analysis in the supervised reader child; only document
reads move to in-process SQLite. The query compiler already emits `dialect="sqlite"`, so SP-02 can execute
that SQL directly with `node:sqlite` without a full backend migration. The reader child, IPC, supervision and
teardown discipline remain load-bearing for ingest and analysis. Read-only document connections and SP-05's
32-permit execution semaphore reduce interference with SQLite writers while bounding concurrent reads. This is
the narrowed outcome of the sqlite-pivot plan's SP-02/SP-05.

## 2026-10-11 — No ORM for Postgres access

Human decision: MT-03 (and any other item touching Postgres) uses the pinned `npm:postgres` client directly
with hand-written SQL, leveraging SQL syntax — not an ORM or query builder generating SQL internally.
Rationale: direct hand-written SQL is tunable and has no hidden query generation. Recorded in
`docs/plans/completed/multitenancy.md` §2.4 and MT-03.

## 2026-10-11 — Knowledge lives in plain markdown, no `kb` CLI

Human decision: `docs/notes/` is markdown with `index.md`, deliberately not the nickstrad/kb full-text store
that `vm-agent` and `perpetual` use. Rationale: zero tooling means any agent (or the human in a text editor)
can read and update entries; nothing to build, index, or reindex.

## 2026-10-11 — Model tier mapping (dual-company)

Sessions run only on Claude or Codex, so every plan item pairs one model per company under a fixed
correspondence: Opus 5.5 ↔ Astra (`gpt-6-astra`) for the difficult items and all reviews; Sonnet ↔ Sol
(`gpt-6.1-sol`) for the middle tier; Haiku ↔ Luna (`gpt-6-luna`, high effort) for the fast tier.
`claude
--model haiku` resolves to `claude-haiku-5-5` (verified 2026-10-11); Haiku ≤4.5 must never be used —
the 4.5 experiment showed it drops safety-critical details and rates itself 5/5 regardless. Canonical source:
repo `AGENTS.md`.

## 2026-10-11 — Multitenancy: Postgres registry, per-tenant SQLite, router interface

`docs/plans/completed/multitenancy.md` (MT-01–MT-10): Postgres is the central tenant registry; each tenant
gets its own SQLite file. Routing is a `TenantStorageRouter` Effect interface the Storage service depends on —
never paths or env vars. `LocalFolderRouter` maps UUID-validated tenants to `TENANTS_DIR/<tenant-id>.db`; a
future `ControlPlaneRouter` must be swappable with zero Storage/client changes. Rationale: grow from local
folder to control plane without rewriting the storage layer.

## 2026-10-11 — Better Auth is not punted

The multitenancy plan opens its auth item (MT-08) with a headless session-token go/no-go spike instead of
deferring auth. Side quest sq-09 proved it viable (better-auth 1.7.7 on Deno 2.9.5 against scratch Postgres:
signup → signin → authenticated 200). Open design point from Opus review: decide token transport before MT-08
builds — `bearer()` plugin if middleware takes `Authorization: Bearer`, else forward the cookie.

## 2026-10-10 — `PROP_SEED` seed-replay convention

Property tests print their failing seed; re-run exactly with `PROP_SEED=<n>`. Coverage assertions and
correctness assertions are reported separately so a coverage miss is never triaged as an invariant break
(lesson from sq-11: every lx-01 "flake" was a coverage assertion, zero invariant defects).

## 2026-10-10 — `agent-work/work-scripts/` library

The human wants work scripts kept: anything an agent writes to investigate, measure, or verify goes in
`agent-work/work-scripts/` (`deno/` for larger work, `bash/` for bash-oriented things), committed and indexed
in `agent-work/work-scripts/INDEX.md` (one row per script: purpose, usage, notes). Rationale, proven
2026-10-11: every side-quest review verdict could be independently re-run in minutes _because_ the scripts,
raw data, and index rows were on the branches.

## 2026-10-10 — Effect port as the architecture

Item 22: the entire server composes as a scoped Effect layer — partial-startup release, shutdown ordering,
ingest-then-sweep RPC triggering. Storage (item 15), DuckDB reader (item 16), object store (item 18), backups
(item 19) are all Effect services. Rationale: typed errors, supervised fibers, and composition replace ad-hoc
lifecycle code.

## 2026-10-10 — SQLite storage, DuckDB analytics, Deno runtime

The MVP's core shape survived the Effect port: SQLite for document storage, DuckDB for event-log ingest and
named analysis, Deno as the runtime. The reflection pass (Astra, 2026-10-11) confirmed the core architecture
sound while sending back data-semantics fixes (items 30–37).

## Standing — Review and landing protocol

Every real plan item gets an independent top-tier review (Opus 5.5 or Astra, a different agent from the
builder) before landing. Nothing merges without Opus review **and** human approval. Landings are reported as
"item N landed and it now gives ability to do \<capability\>".

## 2026-10-11 — Shared agent event inbox, batched Git archive

Agent sessions across linked worktrees need one live log without concurrent index writes. The
[event-log design](../../agent-work/event-log.md) uses Git's common directory for immutable live events and
tracked `agent-work/events/` for coordinator-batched snapshots. Per-event publication uses an atomic hard
link; writers never commit. This is coordination infrastructure, separate from the product's event log.

## 2026-10-11 — MT-08 headless transport and PostgreSQL driver

The mandatory Deno vanilla-client spike passed against Better Auth 1.7.7 and Postgres 17: sign-up, sign-in,
capture the response body token, authenticate a tenant endpoint. The body token returns 401 without the bearer
plugin and authenticates with it, so tenant requests use `Authorization: Bearer <token>` with
`plugins: [bearer()]`. Cookies are not forwarded by tenant middleware.

The pinned built-in PostgreSQL path requires a `pg.Pool`-compatible object, not a raw postgres.js client. To
retain the no-ORM/handwritten-SQL rule, runtime uses Better Auth's minimal entrypoint and a custom adapter
over `npm:postgres@3.4.9`. Only the isolated schema-capture spike uses `pg@8.23.1` with the upstream migration
tooling. Production schema changes remain numbered SQL migrations in the normal `schema_migrations` journal.
This adapter is intentionally limited to the configured core auth models; future plugins/upgrades need adapter
and schema validation.

Membership resolution fails closed unless the user has exactly one membership and that tenant is active. No
client tenant selector is trusted. Auth database handles are owned and closed per request; sharing a pool
requires a future scoped-layer change. See MT-08 implementation notes for executable verification, the
generated schema, and operational limits.
