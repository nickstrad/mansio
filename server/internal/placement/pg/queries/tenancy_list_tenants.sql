-- tenancy_list_tenants.sql — up to $1 tenants, oldest first. Tenants created by the locator's key path have no
-- slug or name: they read as ''.
  select tenant_id,
         coalesce(slug, ''),
         coalesce(name, ''),
         status = 'active',
         created_at
    from tenants
order by created_at, tenant_id
   limit $1;
