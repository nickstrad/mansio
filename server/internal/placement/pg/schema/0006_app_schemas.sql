-- 0006_app_schemas.sql — the app schema registry (IR protocol plan §2.4, IR-2). Applied once by Migrate; never
-- edit after it is applied.
-- An app registers its schema (the defineSchema JSON) once with controld; agentd fetches it by fingerprint when
-- it creates an app's database, and caches it (internal/schemareg). A database is (tenant, app), stored under
-- the placement key tenant ':' app (ADR 0011), so an app is named by the database_name domain.
-- Schemas are content-addressed: the fingerprint is "sha256:" and the lowercase hex SHA-256 of the exact text
-- (ir/spec/v1.md §2), and the schema checks it, whichever code path writes (SQL G-12). Rows are never updated:
-- a changed schema is a new fingerprint.
begin;

create domain schema_fingerprint as text collate "C"
  check (value ~ '^sha256:[0-9a-f]{64}$');

create table schemas (
  fingerprint schema_fingerprint primary key,
  -- The fingerprint text, byte for byte (what _platform_schema.definition stores). 64 KiB: the request cap.
  body        text not null
              constraint schemas_body_size check (octet_length(body) between 2 and 65536),
  created_at  timestamptz not null default now(),
  constraint schemas_fingerprint_is_sha256
    check (fingerprint = 'sha256:' || encode(sha256(convert_to(body, 'UTF8')), 'hex'))
);

-- Which app registered which schema: an agent serves a fingerprint only to the app that registered it.
create table app_schemas (
  app           database_name not null,
  fingerprint   schema_fingerprint not null references schemas (fingerprint),
  registered_at timestamptz not null default now(),
  primary key (app, fingerprint)
);
-- The primary key covers app; index the FK's referencing side (SQL G-12).
create index app_schemas_fingerprint on app_schemas (fingerprint);

insert into schema_version (version) values (6);

commit;
