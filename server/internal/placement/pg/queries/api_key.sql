-- api_key.sql — one API key's row by id: its tenant, principal, permissions and secret hash. A key of a
-- deactivated tenant reads as revoked (0007; deactivation also revokes, this covers rows written otherwise).
-- No row ⇒ locator.ErrNotFound (the locator answers unauthorized).
select k.tenant_id,
       k.principal,
       k.perms,
       k.secret_sha256,
       k.revoked_at is not null or t.status <> 'active' as revoked
  from api_keys as k
  join tenants as t on t.tenant_id = k.tenant_id
 where k.key_id = $1;
