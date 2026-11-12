# Backup, restore and import on the Go server

How a backup, a restore and the import of an existing SQLite file work on agentd, where a database's durable
copy is already a chain of S3 generations. This replaced the Deno platform's `/admin/backup/:appId`,
`scripts/restore.ts` and its plain SQLite files.

## Design

A backup on Go is not a second copy: the write-back already publishes immutable generations named by a
conditional manifest (`server/internal/durable`). A backup is therefore "write back now and tell me which
generation holds everything so far", and a restore is "download that generation".

| Deno platform (deleted)                                   | Go                                                                                                                 |
| --------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| `POST /admin/backup/:appId` (admin token)                 | `POST /admin/backup?key=tenant:app` on agentd's admin listener (loopback; `agentctl backup`)                       |
| answer: `{format, appId, key, sha256, bytes, capturedAt}` | answer: the stored manifest `{key, generation, epoch, object, size, sha256, md5, written_at}` + `version`, `wrote` |
| `latest.json` = the last backup                           | the manifest = the last write-back (periodic, passivation, shutdown, or a backup)                                  |
| `deno task restore <app> <path>`                          | `dbtool restore [-gen G] <key> <path>`                                                                             |
| tenants not backed up                                     | any key: tenant databases are keys like any other                                                                  |
| —                                                         | `dbtool import <file> -tenant T -app A -schema F [-placement-dir D -vm V]`                                         |

**Backup** (`cmd/agentd/backup.go`): only for a key the owner table holds `Active` here (else 409
`wrong_owner`). `Host.WriteBack` snapshots on a reader (no write stall) and publishes if dirty; the key stays
warm. Then one manifest read answers what is stored (never older than what the write-back published, else a
bug: `internal`). A stale write-back is 409 `fenced`, a transient S3 failure 503 `unavailable`.

**Restore** (`cmd/dbtool/restore.go`): without `-gen`, the manifest's generation; with `-gen G` (a backup's
answer), the object of generation G, its manifest rebuilt from the object's metadata so the download is still
verified (size + sha256: never trust a download unverified). Then `integrity_check(1)` on a read-only
`immutable` open, fsync, and a hard link to the target (fails if it exists; the target, `-wal`, `-shm`,
`-journal` must not exist). Nothing in S3 changes. A generation GC deleted (it keeps K = 2 below the current
one past the grace window) is gone: `restore -gen` says so. There is no pinning of backups yet.

**Import** (`cmd/dbtool/import.go`), steps in order, each re-read on an ambiguous answer (an ambiguous write
may have happened, so read before retrying) so a rerun with the same file resumes:

1. `VACUUM INTO` a scratch copy from a `mode=ro` open (the source is not changed; frames still in its `-wal`
   are read: `TestImportReadsTheWAL`), then `integrity_check`.
2. The copy's `_platform_schema.definition` must equal the `-schema` file (fingerprint form, trailing newline
   ignored) byte for byte: that is exactly docexec's fingerprint check, so the agent opens it with no
   `SchemaDriftError`. A mismatch prints both fingerprints.
3. The key must be new: no manifest, and no placement row other than the one this import writes.
4. Upload generation 1 at epoch 1 (`If-None-Match: *`), publish the manifest only if none exists, then CAS the
   placement row `ACTIVE(vm, epoch 1, gen 1)` from "no row" (file authority, `-placement-dir`).

The control plane's claim then serves the key (`Stamp` keeps generation 1). `VACUUM INTO` of an unchanged file
is byte-deterministic (checked), which is what makes a rerun after an interrupted upload resume rather than
refuse. Without `-placement-dir` steps 1–3 and the S3 half of step 4 run: controld's create (Postgres) writes
its row at generation 0 and the owner's claim adopts the manifest (`m.Gen >= p.Gen`).
`TestClaimAdoptsAnImportedGeneration` (`internal/owner`) pins that claim; the controld flow end to end is
covered by `server/cmd/controld/import_gate_test.go` ([multi-node](multi-node.md)). A row written meanwhile
(between import's check and its CAS) that a claim can adopt (Active, epoch >= 1, gen <= 1) is reported as
`adopted`; any other is refused. An orphan generation 1 from another file's interrupted import is refused for
good: gc skips a key with no manifest, so it must be removed by hand.

agentd durable mode also accepts `schemas_dir` (at most one of `schemas_dir`, `schemas_url`), so a single-node
agent creates new databases without controld. `sqlitedsn.ReadOnly(path, immutable)` builds the read-only URIs.

## E2E (`backup_restore`)

`tests/e2e/platform/durable.ts`, `startDurableAgent`: a durable agent per phase under its own S3 prefix,
`-dev`, file placement, `schemas_dir`, tickets minted by the harness (no tenancy auth needed: backup is an
admin-listener control, restore/import are operator CLIs).

1. Claim both apps with `create`, play two moves, back up (`wrote: true`, `dbtool verify` agrees on generation
   and sha256), play a third move, stop (the shutdown writes it back).
2. `dbtool restore -gen <backup>`: same sha256 as the backup's answer; without `-gen`: a newer generation; a
   second restore onto the same path is refused.
3. A second agent imports the restored files, claims (no create) and serves exactly the backed-up game.

`scripts/dev/e2e-linux.sh` cross-compiles `dbtool` (`DBTOOL_BIN`) and starts a SeaweedFS 4.48 sibling
container (`E2E_S3_ENDPOINT=s3:8333`, bucket `e2e`, identity `e2e`/`e2e-secret`); `main.ts` forwards
`DBTOOL_BIN` and `E2E_S3_*`. Without them `backup_restore` fails with a message naming the variables.

## Traps

- Backup answers what is **stored**, not what this call wrote: if a periodic write-back raced it, the
  generation can be newer than the one it uploaded (`wrote` still says whether this call uploaded).
- `restore -gen G` for an old generation trusts the object's own metadata for the expected sha256 (the
  manifest no longer names it); an object without metadata (written before objects carried it) cannot be
  restored by number.
- The harness's durable agent waits for `mansio_s3_probe_ok 1` before claiming: agentd creates the bucket in
  the background, and a claim that stamps the manifest before then would fail (not measured; the wait avoids
  the race).
