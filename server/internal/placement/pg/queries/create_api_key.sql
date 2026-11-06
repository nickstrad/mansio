-- create_api_key.sql — store a new API key ($1 id, $2 tenant, $3 principal, $4 SHA-256 of the secret, $5 perms),
-- creating the tenant if new, in one statement, unless the tenant is deactivated. The tenant row is locked
-- (for share) so a concurrent deactivation waits for this insert, then revokes the key too (as
-- tenancy_create_key.sql). One row: the tenant's status and the key id (null: not inserted, the tenant is
-- deactivated). A duplicate id fails api_keys_pkey (pg.ErrInvariant; the caller draws a new id).
with created as (
     insert into tenants (tenant_id)
          values ($2)
     on conflict (tenant_id) do nothing
       returning status
),
existing as (
     select status
       from tenants
      where tenant_id = $2
        for share
),
tenant as (
     select status from created
      union all
     select status from existing
),
inserted as (
     insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms)
          select $1, $2, $3, $4, $5
           where exists (select 1 from tenant where status = 'active')
       returning key_id
)
select (select status from tenant limit 1),
       (select key_id from inserted);
