-- tenancy_session_tenant.sql — the tenant of the session $1 that is unexpired at $2: always one row, with its
-- user's membership count and whether every member tenant is active. The caller accepts exactly one membership
-- of an active tenant.
select min(m.tenant_id),
       count(m.tenant_id),
       coalesce(bool_and(t.status = 'active'), false)
  from auth_sessions as s
  join memberships as m on m.user_id = s.user_id
  join tenants as t on t.tenant_id = m.tenant_id
 where s.token_sha256 = $1
   and s.expires_at > $2;
