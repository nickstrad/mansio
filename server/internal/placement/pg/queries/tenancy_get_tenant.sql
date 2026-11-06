-- tenancy_get_tenant.sql — one tenant by id ($1). No row ⇒ tenancy.ErrNotFound.
select tenant_id,
       coalesce(slug, ''),
       coalesce(name, ''),
       status = 'active',
       created_at
  from tenants
 where tenant_id = $1;
