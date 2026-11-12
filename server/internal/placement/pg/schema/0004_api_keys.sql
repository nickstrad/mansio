-- 0004_api_keys.sql — API keys, the locator's first principals. Applied once by Migrate; never edit after
-- it is applied.
-- A key is "mansio_<key_id>_<secret>" (internal/control/locator); only SHA-256(secret) is stored, so a leaked
-- table yields no usable key. The locator checks a key once per ticket it issues (cached for seconds), never
-- per query.
begin;

create table api_keys (
  key_id        text collate "C" primary key
                constraint api_keys_key_id_check check (key_id ~ '^[0-9a-f]{16}$'),
  tenant_id     tenant_id not null references tenants (tenant_id),
  -- The ticket's sub claim (ticket.MaxSub = 128 bytes).
  principal     text collate "C" not null
                constraint api_keys_principal_check check (principal ~ '^[a-z0-9][a-z0-9._@-]{0,127}$'),
  secret_sha256 bytea not null
                constraint api_keys_secret_sha256_check check (octet_length(secret_sha256) = 32),
  -- ticket.Perm: 1 read, 2 write, 3 both.
  perms         smallint not null
                constraint api_keys_perms_check check (perms between 1 and 3),
  created_at    timestamptz not null default now(),
  revoked_at    timestamptz
);
-- "keys of tenant t" and the FK on tenant_id.
create index api_keys_tenant on api_keys (tenant_id);

insert into schema_version (version) values (4);

commit;
