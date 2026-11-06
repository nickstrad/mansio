-- 0007_tenancy.sql — mansio's tenancy and auth in the control plane's schema (replace-platform plan §4.3, §4.4,
-- IR-4a). Applied once by Migrate; never edit after it is applied.
-- Tenants gain mansio's admin fields (slug, name, status). API keys gain a name and the legacy format: mansio's
-- Deno platform issued "mansio_<base64url>" keys and stored SHA-256 of the whole key, under a uuid id; those rows
-- are kept as they are (legacy = true, key_id the uuid, secret_sha256 the whole key's hash) so issued keys keep
-- working. New keys are "mansio_<key_id>_<secret>" (internal/control/locator). Users, password credentials,
-- sessions and memberships replace Better Auth's tables: sessions are stored by SHA-256 of their bearer token,
-- never the token.
begin;

alter table tenants
  add column slug   text collate "C" constraint tenants_slug_key unique
                    constraint tenants_slug_check check (octet_length(slug) between 1 and 200),
  add column name   text constraint tenants_name_check check (octet_length(name) between 1 and 200),
  add column status text not null default 'active'
                    constraint tenants_status_check check (status in ('active', 'deactivated'));

alter table api_keys drop constraint api_keys_key_id_check;
alter table api_keys
  add column legacy boolean not null default false,
  add column name   text constraint api_keys_name_check check (octet_length(name) between 1 and 200),
  -- New keys: 16 hex digits. Legacy keys: the Deno platform's canonical lowercase uuid.
  add constraint api_keys_key_id_check check (
    case when legacy then key_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
         else key_id ~ '^[0-9a-f]{16}$' end);
-- A legacy key is found by its hash (it carries no id); new keys by key_id, so only legacy hashes need an index.
create unique index api_keys_legacy_hash on api_keys (secret_sha256) where legacy;

-- Better Auth's "user" table. Emails are stored lowercase (Better Auth lowercases them), so the key compares
-- bytewise.
create table auth_users (
  user_id        text collate "C" primary key
                 constraint auth_users_user_id_check check (user_id ~ '^[A-Za-z0-9_-]{1,64}$'),
  email          text collate "C" not null constraint auth_users_email_key unique
                 constraint auth_users_email_check check (email = lower(email) and octet_length(email) between 3 and 254),
  name           text not null constraint auth_users_name_check check (octet_length(name) between 1 and 200),
  email_verified boolean not null default false,
  image          text,
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now()
);

-- Better Auth's credential "account" rows: one password per user, the scrypt hash "<salt hex>:<key hex>"
-- (N=16384, r=16, p=1, 64-byte key over the NFKC password, the salt's hex text as the salt).
create table auth_passwords (
  user_id    text collate "C" primary key references auth_users (user_id) on delete cascade,
  hash       text not null constraint auth_passwords_hash_check check (hash ~ '^[0-9a-f]{32}:[0-9a-f]{128}$'),
  updated_at timestamptz not null default now()
);

create table auth_sessions (
  token_sha256 bytea primary key
               constraint auth_sessions_token_sha256_check check (octet_length(token_sha256) = 32),
  user_id      text collate "C" not null references auth_users (user_id) on delete cascade,
  expires_at   timestamptz not null,
  created_at   timestamptz not null default now(),
  constraint auth_sessions_expiry_check check (expires_at > created_at)
);
-- "sessions of user u" and the FK on user_id (SQL G-12).
create index auth_sessions_user on auth_sessions (user_id);

-- A session's tenant is its user's only membership (none or several: refused).
create table memberships (
  user_id   text collate "C" not null references auth_users (user_id) on delete cascade,
  tenant_id tenant_id not null references tenants (tenant_id) on delete cascade,
  role      text not null default 'member' constraint memberships_role_check check (role in ('member', 'admin')),
  primary key (user_id, tenant_id)
);
-- The FK on tenant_id (SQL G-12).
create index memberships_tenant on memberships (tenant_id);

insert into schema_version (version) values (7);

commit;
