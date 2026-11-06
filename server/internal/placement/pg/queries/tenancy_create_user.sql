-- tenancy_create_user.sql — store a user ($1 id, $2 email, $3 name, $4 created), its password hash ($5) and its
-- first session ($6 SHA-256 of the token, $7 expiry), in one statement. A taken email fails auth_users_email_key
-- (tenancy.ErrConflict).
with u as (
     insert into auth_users (user_id, email, name, created_at, updated_at)
          values ($1, $2, $3, $4, $4)
       returning user_id
),
p as (
     insert into auth_passwords (user_id, hash)
          select user_id, $5
            from u
),
s as (
     insert into auth_sessions (token_sha256, user_id, expires_at)
          select $6, user_id, $7
            from u
)
select user_id
  from u;
