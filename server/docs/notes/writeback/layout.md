# Durable layout and ambiguity resolution

Where a database's manifest and generations live in the bucket, what metadata each object carries, and how an
upload or stamp whose outcome is unknown is resolved by re-reading.

Rules it follows: keys are spread over hashed prefixes and one key's objects never lie under another key's
prefix; every immutable object describes itself in its metadata; after an ambiguous write, re-read before
deciding, and treat a write as stale only when the re-read shows another writer.

## Layout

The old `dbs/<key>/…` objects are not read any more (no migration: there was no real data).

```
dbs/<h2>/<esc>/manifest.json                          the only mutable object
dbs/<h2>/<esc>/gen/<G 20d>-e<E 20d>.db                immutable, uploaded at epoch E
```

- `h2` = first 2 hex digits of sha256(key); `esc` = `url.PathEscape(key)` ("/" → `%2F`). Built only by
  `durable.KeyPrefix` / `GenObject(key, gen, epoch)` / `ManifestObject`. Example: `t1/a` → `dbs/bb/t1%2Fa/`.
  `FuzzKeyPrefixDisjoint` checks no key's objects lie under another key's prefix.
- One key limit: `durable.MaxKeyLen = 120` (owner uses it; durable allowed 512 before).
- A manifest at epoch M may name an object uploaded at E ≤ M (a Stamp raises M without renaming).
- Generation user metadata: `sha256`, `epoch`, `generation`, `key` (path-escaped), `format=sqlite3-v1`;
  `durable.Store.StatGeneration` returns it (`GenInfo`). Manifest gains optional `md5` (hex).

## Ambiguous writes

- Warm `vactor.Host.Stamp` and cold `durable.Stamp` re-read the manifest after any failed publish; success if
  it is the stamp sent. The warm key is marked stale only when the re-read shows another writer.
- A failed `PutGeneration` is followed by `StatGeneration`; matching size + sha256 metadata ⇒ adopted.
- Fault switches: `durabletest.Faults{ApplyThenStale, LoseUploadReply}`, `s3test.ApplyThenPreconditionFailed`
  (apply, then 412), `ownertest.Objects.LoseNextReply`.
- Evidence: `TestStampLostReplyThroughS3`, `TestLostUploadReplyThroughS3` (vactor over the real adapter and
  s3test) fail on the pre-change code; `FuzzDurableHostModel` seeds with the new faults fail on it too.
- SeaweedFS 4.47 (localhost:8333): `COLDLOAD_S3=localhost:8333 go test ./internal/objstore` passes with the
  new layout and metadata (contract suite incl. a non-ASCII key's metadata round trip).
