# Glossary

Project terms, for agents joining mid-flight.

| Term                            | Meaning                                                                                                                                                                                           |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Plan item                       | Numbered build unit in `docs/plans/effect-port-and-observability.md` (items 01–37). Each names a Claude model and a Codex model per the tier mapping.                                             |
| Batch                           | A group of plan items built in one wave (e.g. Batch 11 = items 30–37, the reflection suggestions).                                                                                                |
| MT-01…MT-10                     | Multitenancy plan items in `docs/plans/multitenancy.md`: Postgres registry, config, tenants, routing, admin, auth (MT-08), property tests (MT-09), hermetic e2e (MT-10).                          |
| Side quest (sq-NN)              | Small bounded investigation on a `haiku/sq-NN` branch: survey, measure, or triage. Findings-only; nothing merges without Opus review and human approval.                                          |
| Luna experiments (luna-_, lx-_) | Blind re-implementations and measurement sweeps run on the Luna (Codex fast-tier) side, reviewed by Opus.                                                                                         |
| Opus review                     | Independent top-tier review (Opus 5.5 or Astra) before any landing. Plan items: MERGE-WORTHY / FINDINGS-ONLY / REJECT. Side quests: USEFUL / MARGINAL / NOT USEFUL.                               |
| PROP_SEED                       | Env var replaying a property-test seed exactly: `PROP_SEED=<n> deno task test:prop`. A failing invariant prints its seed.                                                                         |
| Landing phrasing                | Human's standing format: "item N landed and it now gives ability to do \<capability\>" — every landing reports what it unlocks.                                                                   |
| mansio-project-updates          | 20-minute status cadence the human asked for; every update ends with a Claude usage line (token totals + weekly-limit % left + reset time).                                                       |
| TenantStorageRouter             | Effect interface the Storage service depends on for per-tenant routing (never paths/env vars).                                                                                                    |
| LocalFolderRouter               | Current router: UUID-validated tenants → `TENANTS_DIR/<tenant-id>.db`.                                                                                                                            |
| ControlPlaneRouter              | Future router, swappable with zero Storage/client changes.                                                                                                                                        |
| TENANCY                         | Mode flag: `off` \| `api-key` \| `better-auth` (default `off`).                                                                                                                                   |
| Reflection pass                 | Dedicated architectural review at trajectory inflection points: sync C4 diagrams **and usage docs**, run full tests, run DuckDB analysis queries against real data, produce an evidenced verdict. |
| `work-scripts/`                 | Committed library of investigation/measurement scripts (`deno/`, `bash/`), indexed in `work-scripts/INDEX.md`. The human wants work code kept, not lost in `/tmp`.                                |
| agent-work/                     | Per-item build briefs, reviews, and `COORDINATION.md` (coordinator's live log).                                                                                                                   |
