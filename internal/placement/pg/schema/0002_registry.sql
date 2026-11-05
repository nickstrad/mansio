-- 0002_registry.sql — the control plane's registries (CP-4): tenants, their databases, and what the VM
-- registry needs beyond 0001 (the bytes each VM last reported). Applied once by Migrate; never edit after it
-- is applied. Load per VM is computed by vm_load.sql from placement (no counter columns, SQL G-15).
begin;

create domain tenant_id as text collate "C"
  check (value ~ '^[a-z0-9][a-z0-9-]{0,62}$');
-- A database's name inside its tenant. The placement key is tenant_id || ':' || name (≤ 63 + 1 + 56 = 120
-- bytes, the database_key limit), which front.CheckDBID accepts as a wire database id.
create domain database_name as text collate "C"
  check (value ~ '^[a-z0-9][a-z0-9_-]{0,55}$');

create table tenants (
  tenant_id  tenant_id primary key,
  created_at timestamptz not null default now()
);

-- One row per database ever created; a dropped database keeps its row as a tombstone, so its name is never
-- reused (a new database with an old name would meet the old one's objects and epochs).
--   creating  : row written, placement row and the agent's Claim may still be missing (flow B; resumable)
--   active    : placed and claimed
--   moving    : a move to target_vm is in progress (flow C; resumable from moving.sql)
--   suspended : fenced on its owner, placement MOVING, no target; resume = a move
--   dropping  : the drop tombstone: nothing may serve it; placement row, objects and records are going
--   dropped   : gone; the row stays as a tombstone
create table databases (
  database_id database_key primary key,
  tenant_id   tenant_id not null references tenants (tenant_id),
  name        database_name not null,
  is_default  boolean not null default false,
  state       text not null default 'creating'
              constraint databases_state_check
              check (state in ('creating', 'active', 'moving', 'suspended', 'dropping', 'dropped')),
  target_vm   vm_id references vms (vm_id),
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  constraint databases_key_is_tenant_name check (database_id = tenant_id || ':' || name),
  constraint databases_target_iff_moving check ((target_vm is not null) = (state = 'moving'))
);
-- "databases of tenant t" and the FK on tenant_id (SQL G-12).
create index databases_tenant on databases (tenant_id);
-- FK on target_vm; few rows are moving.
create index databases_target_vm on databases (target_vm) where target_vm is not null;
-- The lifecycle controller resumes unfinished creates and drops from this (a table scan, never NOTIFY).
create index databases_pending on databases (updated_at) where state in ('creating', 'dropping');
-- At most one default database per tenant among the live ones.
create unique index databases_one_default on databases (tenant_id) where is_default and state <> 'dropped';

-- What each VM last reported in its health answer (controld's heartbeat). A gauge the agent measures, not a
-- counter we maintain.
alter table vms add column bytes bigint not null default 0 constraint vms_bytes_check check (bytes >= 0);

insert into schema_version (version) values (2);

commit;
