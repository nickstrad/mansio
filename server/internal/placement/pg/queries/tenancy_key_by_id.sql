-- tenancy_key_by_id.sql — what authenticating a new-format key ($1 id) needs: its tenant, secret hash, whether
-- it is revoked and whether its tenant is active. No row ⇒ tenancy.ErrNotFound.
select k.tenant_id,
       k.secret_sha256,
       k.revoked_at is not null,
       t.status = 'active'
  from api_keys as k
  join tenants as t on t.tenant_id = k.tenant_id
 where k.key_id = $1
   and not k.legacy;
