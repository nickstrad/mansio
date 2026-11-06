-- tenancy_key_by_legacy_hash.sql — the same for a legacy key, found by SHA-256 of the whole key ($1; the
-- api_keys_legacy_hash index). No row ⇒ tenancy.ErrNotFound.
select k.tenant_id,
       k.secret_sha256,
       k.revoked_at is not null,
       t.status = 'active'
  from api_keys as k
  join tenants as t on t.tenant_id = k.tenant_id
 where k.secret_sha256 = $1
   and k.legacy;
