-- tenancy_create_key.sql — store a read+write key ($1 id, $2 tenant, $3 SHA-256 of the secret, $4 name) for an
-- active tenant. The tenant row is locked (for share) so a concurrent deactivation waits for this insert, then
-- revokes the key too. One row: the tenant's status (null: no tenant) and the key's fields (null: not inserted).
with tenant as (
     select tenant_id,
            status
       from tenants
      where tenant_id = $2
        for share
),
inserted as (
     insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, name)
          select $1, tenant_id, $1, $3, 3, $4
            from tenant
           where status = 'active'
       returning key_id, tenant_id, name, created_at
)
   select (select status from tenant),
          inserted.key_id,
          inserted.tenant_id,
          inserted.name,
          inserted.created_at
     from (values (1)) as one (n)
left join inserted on true;
