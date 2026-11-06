# haiku-14 — Opus review: k6 load scenarios and output capture (plan item 14)

**Branch:** `haiku/14-k6` (`7cddbc4`, single commit) **Reviewer:** Opus, 2026-10-10 **Review worktree:**
`origin/haiku/14-k6` at `/root/Software/mansio-work/rev14` (read-only) **Verdict: FAIL** — two of the three
scripts do not load in k6 at all, the required load model and custom metric are missing, and the output
capture the item exists to deliver is dead code.

## What was built

- `perf/mutation_load.js` (80 lines), `perf/query_load.js` (111), `perf/mixed.js` (92)
- `perf/lib/scenarios.js` (96 lines): `generateRunId`, `seedGames`, per-endpoint helpers, `configureOutput`,
  `createHandleSummary`
- `perf/lib/api.js` (44 lines) — **owned by item 03**, see scope below
- `perf/README.md` (102 lines, whole new file)

## Test results I verified

k6 v0.56.0 at `/usr/local/bin/k6`. I used `k6 archive`, which parses and bundles a script plus its module
graph and thresholds without needing a live server — the cheapest faithful check of "syntactically valid and
matches spec":

| Script                  | `k6 archive` exit | Error                                                                                                          |
| ----------------------- | ----------------- | -------------------------------------------------------------------------------------------------------------- |
| `perf/mutation_load.js` | **0**             | —                                                                                                              |
| `perf/query_load.js`    | **104**           | `failed parsing threshold expression's "< 0.01" left hand side; reason: failed parsing method from expression` |
| `perf/mixed.js`         | **104**           | same                                                                                                           |

## Defects

### 1. Two of three scripts cannot run — malformed thresholds (blocking)

`query_load.js:21` and `mixed.js:17` both declare:

```js
"http_req_failed": ["< 0.01"],
```

k6 thresholds require an aggregation method on the left-hand side — `["rate < 0.01"]`. As written, k6 aborts
during setup with exit 104, so `deno task perf:query` and `deno task perf:mixed` cannot reach the server, let
alone pass. The plan's prose reads _"Thresholds: `http_req_failed < 0.01`"_, and Haiku transcribed the prose
literally without the `rate` method — the exact failure mode this item was picked to test ("transcribe a
spec"). It is a one-word fix per file, and any single execution of either task would have caught it.

Acceptance criterion 1 ("`perf:load`, `perf:query`, `perf:mixed` each exit 0 with thresholds met") fails for
two of three scenarios. Criterion 4 (a deliberately low threshold makes the task exit non-zero) is also
unverifiable for those two, since they already exit non-zero unconditionally.

### 2. `query_load.js` uses the wrong load model (blocking)

The plan specifies `constant-arrival-rate` at 50 rps for 2 min. The script instead uses:

```js
options = { stages: [{ duration: "2m", target: 50 }] };
```

That is ramping-VUs to **50 concurrent VUs**, not 50 requests/second. The two are not interchangeable: under a
closed-model VU ramp, throughput collapses to whatever the server can serve, so the scenario silently
self-throttles. This is the one scenario whose entire purpose is to drive a _fixed arrival rate_ at the
serialized single-reader process to find its capacity limit — with a VU model it cannot do that, and p95
becomes a function of the server's own queueing rather than of offered load.

### 3. No custom `conflicts` counter; conflicts land in `http_req_failed` (blocking)

The plan is explicit: a 409 or stale-version error is a _success_ for the mutation scenario and "must be
counted in a custom `conflicts` counter, not in `http_req_failed`." There is no `k6/metrics` import anywhere
on the branch — no `Counter`, `Rate` or `Trend`. The only trace of the requirement is a comment at
`mutation_load.js:45` (`// Make moves with expectedVersion - conflicts
are expected`). Because `api.js` calls
plain `http.post` with no `responseCallback`, a 409 _does_ count as a failed request and is excluded from
`http_req_duration{expected_response:true}`. The requirement is unimplemented.

### 4. Output capture is dead code — `raw.json` is never written (blocking)

Item 14's work item 4 is `--out json=logs/k6/<run-id>/raw.json` wiring plus a `handleSummary` writing
`summary.json`. Neither works:

- `scenarios.js:60 configureOutput(runId)` returns `{logsDir, rawJsonPath}` strings and **is never called** by
  any script. `--out` cannot be set from inside a k6 script in any case; it must be a CLI flag, and
  `deno task perf:load|perf:query|perf:mixed` on `main` are bare `k6 run perf/<script>.js` with no `--out`. So
  `logs/k6/<run-id>/raw.json` is never produced, and acceptance criterion 2 fails. (The tasks are in item 01's
  landed `deno.json`; item 14 owns neither the tasks nor `--out`, so the correct move was to flag the gap, not
  to leave dead helpers.)
- `createHandleSummary()` builds `summary = { timestamp, passed: true }` with **`passed` hardcoded true**, so
  a threshold-failing run would still record `passed: true`. It also keys the output on `__ENV.RUN_ID` and
  logs an error when unset — which is the actual runtime state, see defect 5.

### 5. The run id never reaches the trace header

Each script does `const runId = __ENV.RUN_ID || generateRunId(); __ENV.RUN_ID = runId;` at lines 7-8, while
`api.js:9` captures `const runId = __ENV.RUN_ID || ""` at module scope. ES module imports are evaluated before
the importing module's body, so `api.js` reads `__ENV.RUN_ID` _before_ line 8 assigns it, and binds `""`.
Every request therefore sends `X-Trace-Id: ${__VU}-${__ITER}-` with the run id missing. It is non-empty by the
letter of acceptance criterion 3, but useless for its purpose — correlating one run's requests — and it breaks
the k6↔platform join that item 25 is built on. Compounding this, each VU re-runs the init context, so
`generateRunId()` would mint a _different_ id per VU even if the ordering worked.

I did not run the scripts against a live server: `mutation_load.js` is the only one that loads, and its own
acceptance path depends on the `conflicts` counter and `raw.json` capture that do not exist.

### 6. Scope: `perf/lib/api.js` collides with item 03 — **item 03's copy must win**

Flagging this explicitly as instructed. Item 14 owns `perf/mutation_load.js`, `perf/query_load.js`,
`perf/mixed.js`, `perf/lib/scenarios.js`, and _the scenario section of_ `perf/README.md`.

- **`perf/lib/api.js` is owned by item 03** and is listed under item 14's _Reads_. Haiku wrote its own 44-line
  copy. `main` carries no `perf/` files yet, so item 03 has not landed and there was no conflict at author
  time — but **on any future merge, item 03's `perf/lib/api.js` must win and Haiku's copy must be dropped.**
  Haiku's version additionally carries the defect in item 5 above, and it `throw`s on any status ≥ 400, which
  is incompatible with the mutation scenario's expected-409 design; item 03's version is the one to keep.
- **`perf/README.md` is also owned by item 03.** Item 14 was scoped to contribute only the scenario section;
  Haiku created the entire 102-line file. On merge, item 03 owns the file and only Haiku's scenario section
  should be carried over.

## Scorecard verdict

**Conversion verdict: not acceptable for this item class — keep on Sonnet/Opus.** This was the most mechanical
of the three items — "three scripts repeating a pattern item 03 already established" — and it is the one that
failed hardest. Two of three scripts never load; the custom metric, the load model and the output capture that
constitute the item are all absent or inert. Nothing here was executed before commit: a single `k6 run` would
have surfaced defect 1 in under a second.
