-- tenancy_password.sql — the user with email $1 and its password hash. No row ⇒ tenancy.ErrNotFound.
select u.user_id,
       u.email,
       u.name,
       u.email_verified,
       u.created_at,
       u.updated_at,
       p.hash
  from auth_users as u
  join auth_passwords as p on p.user_id = u.user_id
 where u.email = $1;
