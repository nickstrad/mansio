-- import_mansio_core.sql — on the Deno platform's database (migration 001): every tenant and API key as one JSON
-- document {tenants: […], api_keys: […]}, for import_mansio.sql. key_hash is SHA-256 hex of the whole key.
select json_build_object(
         'tenants', coalesce((select json_agg(json_build_object(
                                 'id', t.id::text, 'slug', t.slug, 'name', t.name,
                                 'status', coalesce(t.status, 'active'), 'created_at', t.created_at)
                               order by t.created_at, t.id)
                                from tenants as t), '[]'),
         'api_keys', coalesce((select json_agg(json_build_object(
                                  'id', k.id::text, 'tenant_id', k.tenant_id::text, 'hash', k.key_hash,
                                  'name', k.name, 'created_at', k.created_at, 'revoked_at', k.revoked_at)
                                order by k.created_at, k.id)
                                 from api_keys as k), '[]'));
