-- api_key.sql — one API key's row by id: its tenant, principal, permissions and secret hash. No row ⇒
-- locator.ErrNotFound (the locator answers unauthorized).
select tenant_id,
       principal,
       perms,
       secret_sha256,
       revoked_at is not null as revoked
  from api_keys
 where key_id = $1;
