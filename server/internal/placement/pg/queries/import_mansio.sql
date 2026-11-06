-- import_mansio.sql — write the Deno platform's tenancy ($1: import_mansio_core.sql's document merged with
-- import_mansio_auth.sql's) into the control plane's tables, in one statement: tenants, legacy API keys (their
-- whole-key hash, perms read+write), users, passwords, sessions, memberships. A re-run adds what is new and
-- carries the changes that only ever go one way: a tenant deactivated at the source is deactivated here (and
-- every new-format key it has here is revoked), a key revoked at the source is revoked here (the first time
-- stays), a changed password hash replaces the old one (the Go backend has no password change of its own yet).
-- It never reactivates, unrevokes or deletes: a membership or session removed at the source stays here. Foreign
-- keys are checked at the end of the statement, after every CTE has written. One row: the counts.
with src as (
     select $1::jsonb as d
),
t as (
     insert into tenants (tenant_id, slug, name, status, created_at)
          select x->>'id', x->>'slug', nullif(x->>'name', ''), x->>'status',
                 coalesce((x->>'created_at')::timestamptz, now())
            from src
           cross join jsonb_array_elements(src.d->'tenants') as x
     on conflict (tenant_id) do update
             set status = 'deactivated'
           where excluded.status = 'deactivated'
             and tenants.status <> 'deactivated'
       returning tenant_id, xmax = 0 as inserted, status
),
-- Deactivation revokes the tenant's keys (as on the Deno platform). Legacy keys carry their own revocation
-- from the source (k), so only new-format keys are touched here: no row is updated twice in one statement.
revoked_by_tenant as (
     update api_keys
        set revoked_at = now()
      where tenant_id in (select tenant_id from t where not inserted and status = 'deactivated')
        and not legacy
        and revoked_at is null
  returning 1
),
k as (
     insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy, name, created_at, revoked_at)
          select x->>'id', x->>'tenant_id', x->>'id', decode(x->>'hash', 'hex'), 3, true, nullif(x->>'name', ''),
                 coalesce((x->>'created_at')::timestamptz, now()), (x->>'revoked_at')::timestamptz
            from src
           cross join jsonb_array_elements(src.d->'api_keys') as x
     on conflict (key_id) do update
             set revoked_at = excluded.revoked_at
           where api_keys.legacy
             and api_keys.tenant_id = excluded.tenant_id
             and api_keys.revoked_at is null
             and excluded.revoked_at is not null
       returning xmax = 0 as inserted
),
u as (
     insert into auth_users (user_id, email, name, email_verified, image, created_at, updated_at)
          select x->>'id', x->>'email', coalesce(nullif(x->>'name', ''), x->>'email'),
                 coalesce((x->>'verified')::boolean, false), x->>'image',
                 (x->>'created_at')::timestamptz, (x->>'updated_at')::timestamptz
            from src
           cross join jsonb_array_elements(coalesce(src.d->'users', '[]')) as x
     on conflict do nothing
       returning 1
),
p as (
     insert into auth_passwords (user_id, hash)
          select x->>'user_id', x->>'hash'
            from src
           cross join jsonb_array_elements(coalesce(src.d->'passwords', '[]')) as x
     on conflict (user_id) do update
             set hash = excluded.hash,
                 updated_at = now()
           where auth_passwords.hash <> excluded.hash
       returning xmax = 0 as inserted
),
s as (
     insert into auth_sessions (token_sha256, user_id, created_at, expires_at)
          select decode(x->>'hash', 'hex'), x->>'user_id', (x->>'created_at')::timestamptz,
                 (x->>'expires_at')::timestamptz
            from src
           cross join jsonb_array_elements(coalesce(src.d->'sessions', '[]')) as x
     on conflict do nothing
       returning 1
),
m as (
     insert into memberships (user_id, tenant_id, role)
          select x->>'user_id', x->>'tenant_id', x->>'role'
            from src
           cross join jsonb_array_elements(coalesce(src.d->'memberships', '[]')) as x
     on conflict do nothing
       returning 1
)
select (select count(*) from t where inserted) as tenants,
       (select count(*) from t where not inserted) as tenants_deactivated,
       (select count(*) from k where inserted) as api_keys,
       (select count(*) from k where not inserted) + (select count(*) from revoked_by_tenant) as api_keys_revoked,
       (select count(*) from u) as users,
       (select count(*) from p where inserted) as passwords,
       (select count(*) from p where not inserted) as passwords_changed,
       (select count(*) from s) as sessions,
       (select count(*) from m) as memberships;
