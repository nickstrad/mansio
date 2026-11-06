-- Central multitenancy schema (MT-01). Idempotent: safe to re-apply. Applied by `make db-migrate`.

CREATE TABLE IF NOT EXISTS tenants (
  id         uuid PRIMARY KEY,
  slug       text UNIQUE NOT NULL,
  name       text,
  status     text DEFAULT 'active',
  created_at timestamptz DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_keys (
  id         uuid PRIMARY KEY,
  tenant_id  uuid NOT NULL REFERENCES tenants (id),
  key_hash   text UNIQUE NOT NULL,
  prefix     text,
  name       text,
  revoked_at timestamptz,
  created_at timestamptz DEFAULT now()
);

CREATE INDEX IF NOT EXISTS api_keys_key_hash_idx ON api_keys (key_hash);
