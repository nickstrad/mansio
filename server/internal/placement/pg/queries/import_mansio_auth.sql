-- import_mansio_auth.sql — on the Deno platform's database (migration 002, Better Auth 1.7): users, their
-- password credentials, unexpired sessions (by SHA-256 of the token: the token itself never leaves this query)
-- and memberships, as one JSON document for import_mansio.sql. Other account providers are not imported (the
-- platform enables only email/password).
select json_build_object(
         'users', coalesce((select json_agg(json_build_object(
                               'id', u.id, 'email', lower(u.email), 'name', u.name,
                               'verified', u."emailVerified", 'image', u.image,
                               'created_at', u."createdAt", 'updated_at', u."updatedAt")
                             order by u."createdAt", u.id)
                              from "user" as u), '[]'),
         'passwords', coalesce((select json_agg(json_build_object('user_id', a."userId", 'hash', a.password)
                                 order by a."userId")
                                  from account as a
                                 where a."providerId" = 'credential'
                                   and a.password is not null), '[]'),
         'sessions', coalesce((select json_agg(json_build_object(
                                  'hash', encode(sha256(convert_to(s.token, 'UTF8')), 'hex'),
                                  'user_id', s."userId", 'created_at', s."createdAt", 'expires_at', s."expiresAt")
                                order by s."createdAt", s.id)
                                 from session as s
                                where s."expiresAt" > now()), '[]'),
         'memberships', coalesce((select json_agg(json_build_object(
                                     'user_id', m.user_id, 'tenant_id', m.tenant_id::text, 'role', m.role)
                                   order by m.user_id, m.tenant_id)
                                    from memberships as m), '[]'));
