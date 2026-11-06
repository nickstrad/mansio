-- import_mansio_has_auth.sql — on the Deno platform's database: whether its auth tables (migration 002:
-- "user", account, session, memberships) are installed.
select to_regclass('"user"') is not null
   and to_regclass('memberships') is not null;
