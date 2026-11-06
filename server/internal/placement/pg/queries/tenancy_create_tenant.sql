-- tenancy_create_tenant.sql — store an active tenant ($1 id, $2 slug, $3 name). A taken slug fails
-- tenants_slug_key (tenancy.ErrConflict).
insert into tenants (tenant_id, slug, name, status)
     values ($1, $2, $3, 'active')
  returning tenant_id,
            coalesce(slug, ''),
            coalesce(name, ''),
            status = 'active',
            created_at;
