-- app_registered.sql — whether app $1 registered any schema (it may then have databases created on first use).
select exists (select 1
                 from app_schemas
                where app = $1) as registered;
