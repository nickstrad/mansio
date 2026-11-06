-- tenancy_link.sql — make the user with email $1 a member of tenant $2 if the tenant is active (idempotent). The
-- tenant row is locked (for share) against a concurrent deactivation. One row: how many users have the email
-- (0 or 1) and the tenant's status (null: no tenant).
with u as (
     select user_id
       from auth_users
      where email = $1
),
t as (
     select tenant_id,
            status
       from tenants
      where tenant_id = $2
        for share
),
linked as (
     insert into memberships (user_id, tenant_id)
          select u.user_id, t.tenant_id
            from u
           cross join t
           where t.status = 'active'
     on conflict (user_id, tenant_id) do nothing
)
select (select count(*) from u),
       (select status from t);
