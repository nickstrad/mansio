-- revoke_api_key.sql — revoke key $1 (idempotent: the first revocation time stays). No row ⇒ owner.ErrNotFound.
   update api_keys
      set revoked_at = coalesce(revoked_at, now())
    where key_id = $1
returning key_id;
