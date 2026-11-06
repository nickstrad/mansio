-- tenancy_deactivate_tenant.sql — deactivate tenant $1 and revoke its keys, in one statement (idempotent: a
-- revocation keeps its first time). A reactivated tenant's old keys would stay dead. No row ⇒ tenancy.ErrNotFound.
with revoked as (
     update api_keys
        set revoked_at = now()
      where tenant_id = $1
        and revoked_at is null
)
   update tenants
      set status = 'deactivated'
    where tenant_id = $1
returning tenant_id,
          coalesce(slug, ''),
          coalesce(name, ''),
          status = 'active',
          created_at;
