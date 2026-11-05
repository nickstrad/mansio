-- 0001_placement.sql — the placement authority (owner.Placement). Applied once by Migrate, or by hand with
-- psql -X -v ON_ERROR_STOP=1 -f 0001_placement.sql. Never edit after it is applied; add 0002_*.sql.
-- Safety rests on: one CAS per change, epochs never decrease, owner changes only with an epoch increase
-- (knowledge/fencing/2026-11-05-safety-argument.md, step 1). The schema enforces all three, whichever code
-- path writes (SQL G-12).
begin;

create domain database_key as text collate "C"
  check (octet_length(value) between 1 and 120);            -- owner.MaxKeyLen
create domain vm_id as text collate "C"
  check (value ~ '^[a-z0-9][a-z0-9.-]{0,62}$');
create domain epoch as bigint check (value >= 1);
create domain generation as bigint check (value >= 0);

create table vms (
  vm_id            vm_id primary key,
  endpoint         text not null,
  registered       timestamptz not null default now(),
  heartbeat        timestamptz not null default now(),   -- updated often: not indexed, so updates stay HOT
  dead_attested_at timestamptz,                          -- operator attests VM loss (M5); null = alive
  dead_attested_by text,
  constraint vms_dead_attested_pair check ((dead_attested_at is null) = (dead_attested_by is null))
) with (fillfactor = 70);

create table placement (
  database_id database_key primary key,
  owner_vm    vm_id      not null references vms (vm_id),
  epoch       epoch      not null,
  generation  generation not null,
  state       text       not null constraint placement_state_check check (state in ('ACTIVE', 'MOVING')),
  updated_at  timestamptz not null default now()
);
-- Placement policy and VM-loss restore ask "what does VM x own?"; FKs are not indexed automatically.
create index placement_owner_vm on placement (owner_vm);
-- The lifecycle controller resumes stuck moves; few rows are MOVING at a time.
create index placement_moving on placement (updated_at) where state = 'MOVING';

-- One row per completed flip (MOVING on from_vm → ACTIVE on to_vm), written by cas.sql in the same
-- statement as the flip: the VM-loss decision reads ack_generation, so it is never unknown.
create table moves (
  database_id    database_key not null references placement (database_id),
  from_epoch     epoch        not null,
  from_vm        vm_id        not null references vms (vm_id),
  to_vm          vm_id        not null references vms (vm_id),
  ack_generation generation   not null,
  flipped_at     timestamptz  not null default now(),
  primary key (database_id, from_epoch),
  constraint moves_distinct_vms check (from_vm <> to_vm)
);
-- The primary key covers database_id; index the other referencing columns (SQL G-12).
create index moves_from_vm on moves (from_vm);
create index moves_to_vm on moves (to_vm);

-- The authority's invariants as a trigger: a CHECK cannot compare the old and new rows. It raises a named
-- constraint so the adapter tells a bug (this) from a CAS conflict (0 rows).
create function placement_epoch_monotonic() returns trigger language plpgsql as $$
begin
  if new.epoch < old.epoch
     or (new.owner_vm <> old.owner_vm and new.epoch <= old.epoch) then
    raise exception 'placement %: % at epoch % -> % at epoch %',
      old.database_id, old.owner_vm, old.epoch, new.owner_vm, new.epoch
      using errcode = 'check_violation', constraint = 'placement_epoch_monotonic';
  end if;
  return new;
end $$;

create trigger placement_epoch_monotonic
  before update on placement
  for each row execute function placement_epoch_monotonic();

create table schema_version (
  version    int primary key,
  applied_at timestamptz not null default now()
);
insert into schema_version (version) values (1);

commit;
