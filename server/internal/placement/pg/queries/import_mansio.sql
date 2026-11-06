-- import_mansio.sql — write the Deno platform's tenancy ($1: import_mansio_core.sql's document merged with
-- import_mansio_auth.sql's) into the control plane's tables, in one statement: tenants, legacy API keys (their
-- whole-key hash, perms read+write), users, passwords, sessions, memberships. Rows already present (by key) are
-- left as they are, so a re-run imports only what is new. Foreign keys are checked at the end of the statement,
-- after every CTE has inserted. One row: how many rows each table gained.
with src as (
     select $1::jsonb as d
),
t as (
     insert into tenants (tenant_id, slug, name, status, created_at)
          select x->>'id', x->>'slug', nullif(x->>'name', ''), x->>'status',
                 coalesce((x->>'created_at')::timestamptz, now())
            from src
           cross join jsonb_array_elements(src.d->'tenants') as x
     on conflict (tenant_id) do nothing
       returning 1
),
k as (
     insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy, name, created_at, revoked_at)
          select x->>'id', x->>'tenant_id', x->>'id', decode(x->>'hash', 'hex'), 3, true, nullif(x->>'name', ''),
                 coalesce((x->>'created_at')::timestamptz, now()), (x->>'revoked_at')::timestamptz
            from src
           cross join jsonb_array_elements(src.d->'api_keys') as x
     on conflict do nothing
       returning 1
),
u as (
     insert into auth_users (user_id, email, name, email_verified, image, created_at, updated_at)
          select x->>'id', x->>'email', left(coalesce(nullif(x->>'name', ''), x->>'email'), 200),
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
     on conflict do nothing
       returning 1
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
select (select count(*) from t) as tenants,
       (select count(*) from k) as api_keys,
       (select count(*) from u) as users,
       (select count(*) from p) as passwords,
       (select count(*) from s) as sessions,
       (select count(*) from m) as memberships;
