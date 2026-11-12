# 2026-11-05 — storage runbook (plan OS-12)

By the p4-gc agent. One entry per storage failure mode, each with its `cmd/dbtool` command. dbtool reads
credentials from `DBTOOL_S3_ACCESS_KEY` / `DBTOOL_S3_SECRET_KEY` and needs List and Delete (the `gc`
identity of guide S1, not an agent's). Flags: `-endpoint`, `-bucket`, `-root`, `-keep` (K, default 2),
`-grace` (default 10m = 2 × the agents' `WriteBackTimeout`), `-timeout`. Exit 0 ok, 1 failure (with a log ID
in the JSON log on stderr), 2 usage.

```sh
go build -o bin/dbtool ./cmd/dbtool
export DBTOOL_S3_ACCESS_KEY=… DBTOOL_S3_SECRET_KEY=…
bin/dbtool inspect t1/orders          # manifest, version, every generation with metadata and role
```

Rules that hold in every entry: never edit `manifest.json` by hand; never delete a prefix of a database
placement still names (guide GC5a); repairs publish conditionally (If-None-Match, or If-Match on the version
`inspect` showed), so a concurrent owner wins and the repair fails rather than forking history.

## 1. Corrupt generation

Symptom: activations fail with `durable: generation does not match its manifest` (`ErrCorrupt`), the key
tombstones and retries.

1. `dbtool verify <key>`: exit 1 confirms it (metadata mismatch, or size/sha256 of the download).
2. `dbtool inspect <key>`: the K retained generations below the current one are the candidates.
3. Stop the owner from serving the key (placement: move it away or pause it), and take a **new epoch** from
   placement: a rollback to an older generation must raise the epoch (W3: (epoch, generation) strictly
   increases).
4. `dbtool rebuild-manifest -gen <G-1> -epoch <new> -if-version <version from inspect> <key>`. It downloads
   and verifies the chosen generation before it publishes.
5. `dbtool verify <key>`, then let placement admit the owner at the new epoch. Writes after generation G-1
   that only G held are lost: say so in the incident.

## 2. Lost manifest (or unreadable)

Symptom: activations fail with "it has no manifest" (`ErrNoManifest`) for a database that existed, or the
read fails with `ErrBadManifest` ("stored manifest is unreadable"); `inspect` shows `manifest none` or
`UNREADABLE version …`.

1. `dbtool inspect <key>`: pick the highest generation whose metadata names this key (the epoch in its name
   and metadata is the epoch that uploaded it). Orphans above the last published one may exist: prefer the
   one the owner's logs show as published; if unknown, the highest that verifies.
2. Epoch: the placement row's current epoch for the key (never lower than the object's own epoch).
3. Lost: `dbtool rebuild-manifest -gen G -epoch E <key>` (If-None-Match: fails if a manifest appeared).
   Unreadable: add `-if-version <the version inspect listed>`; only that exact version is replaced.
4. `dbtool verify <key>`; activate.

## 3. Orphan growth / bucket bytes growing

Symptom: bucket bytes per database above (K+1) × its size; `Host.Stats().RetentionFailures` rising; many
`gc deletes` rows in `inspect`.

1. `dbtool orphans <key>` or `dbtool gc -dry-run -shard <h2>`: what the sweeper would delete, with sizes.
2. `dbtool gc -shard <h2>` (or `-all`): deletes only objects uploaded more than `-grace` before the
   manifest's last rewrite and more than `-grace` ago, never the current one or the K below it, never the
   manifest. A key whose manifest moved during the sweep is skipped (`manifest moved`): rerun.
3. If retention failures persist, check the agents' credential can Delete and the breaker state.
4. Do not shorten `-grace` below 2 × the agents' `WriteBackTimeout`: an in-flight publish could name an
   object it deletes.

## 4. Cost spike

Symptom: request counts or bytes per operation jump (`objstore.Store.Metrics()`: by op and kind; retried
attempts log `S3.REQUEST_RETRIED`).

1. Which op? `put_gen` + `publish` up: write-backs more often than the policy says (look at `Tick` reports,
   `WriteBackCommits` / `WriteBackEvery`). `fetch` up: activation churn (idle eviction too eager; turn on
   `GenCacheBytes`). `list` up: someone runs gc too often (it is the only LIST user besides setup).
2. Retries (kind `transient`, `Retried`) up: see §5.
3. Bytes in the bucket up: see §3.

## 5. SlowDown storm (503 / 429)

Symptom: `S3.REQUEST_RETRIED` with kind `transient` and code `SlowDown`; the breaker opens
(`S3.BREAKER_OPEN`); activations return 503 with `Retry-After`.

1. Do not add retries anywhere: objstore already retries idempotent requests 5 times with jittered backoff,
   and writes are never blindly retried.
2. Reduce load: lower `MaxFetches` / `MaxFetchBytes` (fetch bulkhead), lengthen `WriteBackEvery`, stop any
   `dbtool gc -all` run (LIST + DELETE batches add to the same prefix partitions).
3. Hot prefix? Keys share the 256 `dbs/<h2>/` shards; one hot database is one prefix (~3,500 PUT/s, 5,500
   GET/s on AWS). Write-back rate per key is the lever.
4. After it clears, the breaker heals by itself on the next probe; `dbtool verify` a few affected keys.
