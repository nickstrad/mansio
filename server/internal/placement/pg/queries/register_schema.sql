-- register_schema.sql — register schema $3 (text) under fingerprint $2 for app $1, in one statement. Idempotent:
-- the text is stored once per fingerprint, the app's registration once per (app, fingerprint). created says
-- whether this call added the app's registration. A text whose SHA-256 is not $2 fails
-- schemas_fingerprint_is_sha256 (pg.ErrInvariant; the adapter checks it before sending).
with stored as (
     insert into schemas (fingerprint, body)
          values ($2, $3)
     on conflict (fingerprint) do nothing
),
     registered as (
     insert into app_schemas (app, fingerprint)
          values ($1, $2)
     on conflict (app, fingerprint) do nothing
       returning app
)
select exists (select 1 from registered) as created;
