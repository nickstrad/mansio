-- create_api_key.sql — store a new API key ($1 id, $2 tenant, $3 principal, $4 SHA-256 of the secret, $5 perms),
-- creating the tenant if new, in one statement. A duplicate id fails api_keys_pkey (pg.ErrInvariant; the
-- caller draws a new id).
with tenant as (
     insert into tenants (tenant_id)
          values ($2)
     on conflict (tenant_id) do nothing
)
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms)
     values ($1, $2, $3, $4, $5)
  returning key_id;
