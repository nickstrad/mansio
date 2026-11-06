-- import_mansio_email_taken.sql — the source users ($1: a JSON array of {id, email}) whose email another user
-- already has here: importing them would fail (auth_users_email_key) without naming who. At most 20.
select x->>'email'
  from jsonb_array_elements($1::jsonb) as x
  join auth_users as u on u.email = x->>'email'
 where u.user_id <> x->>'id'
 order by 1
 limit 20;
