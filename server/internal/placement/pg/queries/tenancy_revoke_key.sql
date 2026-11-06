-- tenancy_revoke_key.sql — revoke key $1 if it belongs to tenant $2 (idempotent: the first time stays; a key of
-- another tenant, or none, changes nothing).
update api_keys
   set revoked_at = coalesce(revoked_at, now())
 where key_id = $1
   and tenant_id = $2;
