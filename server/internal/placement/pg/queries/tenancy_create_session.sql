-- tenancy_create_session.sql — store a session ($1 SHA-256 of the token, $2 user, $3 expiry) and reap the
-- user's expired ones (SQL G-15: the sign-in that adds a row removes the dead ones).
with reaped as (
     delete from auth_sessions
      where user_id = $2
        and expires_at <= now()
)
insert into auth_sessions (token_sha256, user_id, expires_at)
     values ($1, $2, $3);
