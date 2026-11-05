-- constraints.sql — each constraint of 0001_placement.sql rejects its off point and accepts its on point
-- (SQL G-20, G-21). Run in a fresh schema after the migrations; every statement autocommits, so a refused
-- statement leaves nothing behind. Golden output: constraints.expected (TestConstraintsGolden, -update).
-- By hand: psql -X -v ON_ERROR_STOP=0 -a -f constraints.sql (psql's error text differs in layout).

-- Fixtures: two VMs.
insert into vms (vm_id, endpoint) values ('vm-a', 'a:1'), ('vm-b', 'b:1');

-- vm_id domain: on point 63 chars from [a-z0-9.-]; off points: 64 chars, uppercase, leading dot, empty.
insert into vms (vm_id, endpoint) values ('v23456789012345678901234567890123456789012345678901234567890123', 'x');
insert into vms (vm_id, endpoint) values ('v234567890123456789012345678901234567890123456789012345678901234', 'x');
insert into vms (vm_id, endpoint) values ('Vm-c', 'x');
insert into vms (vm_id, endpoint) values ('.vm', 'x');
insert into vms (vm_id, endpoint) values ('', 'x');

-- vms_dead_attested_pair: attested time and attester go together.
update vms set dead_attested_at = now() where vm_id = 'vm-b';
update vms set dead_attested_by = 'ops' where vm_id = 'vm-b';
update vms set dead_attested_at = now(), dead_attested_by = 'ops' where vm_id = 'vm-b';
update vms set dead_attested_at = null, dead_attested_by = null where vm_id = 'vm-b';

-- database_key domain: on points 1 and 120 bytes; off points 0 and 121 bytes (octets, not characters).
insert into placement (database_id, owner_vm, epoch, generation, state) values ('k', 'vm-a', 1, 0, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values (repeat('k', 120), 'vm-a', 1, 0, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values ('', 'vm-a', 1, 0, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values (repeat('k', 121), 'vm-a', 1, 0, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values (repeat('é', 60), 'vm-a', 1, 0, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values (repeat('é', 61), 'vm-a', 1, 0, 'ACTIVE');

-- "C" collation: keys compare bytewise, so case differs.
insert into placement (database_id, owner_vm, epoch, generation, state) values ('K', 'vm-a', 1, 0, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values ('k', 'vm-a', 1, 0, 'ACTIVE');

-- epoch domain (≥ 1), generation domain (≥ 0), state check, not null, owner FK.
insert into placement (database_id, owner_vm, epoch, generation, state) values ('e0', 'vm-a', 0, 0, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values ('g-1', 'vm-a', 1, -1, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values ('st', 'vm-a', 1, 0, 'GONE');
insert into placement (database_id, owner_vm, epoch, generation, state) values ('nn', 'vm-a', 1, null, 'ACTIVE');
insert into placement (database_id, owner_vm, epoch, generation, state) values ('fk', 'vm-z', 1, 0, 'ACTIVE');

-- placement_epoch_monotonic: same owner may keep its epoch; the epoch never decreases; a new owner needs a
-- higher epoch (on point: epoch + 1).
update placement set generation = 3 where database_id = 'k';
update placement set state = 'MOVING' where database_id = 'k';
update placement set epoch = 5 where database_id = 'k';
update placement set epoch = 4 where database_id = 'k';
update placement set owner_vm = 'vm-b' where database_id = 'k';
update placement set owner_vm = 'vm-b', epoch = 4 where database_id = 'k';
update placement set owner_vm = 'vm-b', epoch = 6, state = 'ACTIVE' where database_id = 'k';

-- moves: FKs to placement and vms, distinct VMs, one row per (key, from_epoch), ack_generation required.
insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('k', 5, 'vm-a', 'vm-b', 3);
insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('k', 5, 'vm-a', 'vm-b', 3);
insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('k', 4, 'vm-a', 'vm-a', 3);
insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('nope', 1, 'vm-a', 'vm-b', 0);
insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('k', 3, 'vm-a', 'vm-z', 0);
insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('k', 2, 'vm-a', 'vm-b', null);
insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('k', 0, 'vm-a', 'vm-b', 0);

-- A placement row that a move names cannot be deleted from under it.
delete from placement where database_id = 'k';

-- 0002 registries. tenant_id domain: on point 63 chars; off points 64 chars, uppercase, leading dash.
insert into tenants (tenant_id) values ('t23456789012345678901234567890123456789012345678901234567890123');
insert into tenants (tenant_id) values ('t234567890123456789012345678901234567890123456789012345678901234');
insert into tenants (tenant_id) values ('Acme');
insert into tenants (tenant_id) values ('-acme');
insert into tenants (tenant_id) values ('acme');

-- database_name domain: on point 56 chars; off points 57 chars, a colon. The key must be tenant ':' name.
insert into databases (database_id, tenant_id, name) values ('acme:' || repeat('n', 56), 'acme', repeat('n', 56));
insert into databases (database_id, tenant_id, name) values ('acme:' || repeat('n', 57), 'acme', repeat('n', 57));
insert into databases (database_id, tenant_id, name) values ('acme:a:b', 'acme', 'a:b');
insert into databases (database_id, tenant_id, name) values ('acme:x', 'acme', 'y');
insert into databases (database_id, tenant_id, name) values ('nobody:x', 'nobody', 'x');

-- databases_state_check, and databases_target_iff_moving (on points: moving with a target, active without).
insert into databases (database_id, tenant_id, name, state) values ('acme:s', 'acme', 's', 'gone');
insert into databases (database_id, tenant_id, name, state) values ('acme:m', 'acme', 'm', 'moving');
insert into databases (database_id, tenant_id, name, state, target_vm) values ('acme:m', 'acme', 'm', 'active', 'vm-a');
insert into databases (database_id, tenant_id, name, state, target_vm) values ('acme:m', 'acme', 'm', 'moving', 'vm-z');
insert into databases (database_id, tenant_id, name, state, target_vm) values ('acme:m', 'acme', 'm', 'moving', 'vm-a');

-- databases_one_default: one live default per tenant; a dropped default frees the slot.
insert into databases (database_id, tenant_id, name, is_default, state) values ('acme:d1', 'acme', 'd1', true, 'active');
insert into databases (database_id, tenant_id, name, is_default, state) values ('acme:d2', 'acme', 'd2', true, 'active');
update databases set state = 'dropped' where database_id = 'acme:d1';
insert into databases (database_id, tenant_id, name, is_default, state) values ('acme:d2', 'acme', 'd2', true, 'active');

-- vms_bytes_check: on point 0, off point -1.
update vms set bytes = 0 where vm_id = 'vm-a';
update vms set bytes = -1 where vm_id = 'vm-a';

-- What is left.
select database_id, owner_vm, epoch, generation, state from placement order by database_id;
select database_id, from_epoch, from_vm, to_vm, ack_generation from moves order by database_id, from_epoch;
select vm_id, dead_attested_at is null as alive from vms order by vm_id;
select database_id, is_default, state, target_vm from databases order by database_id;
select version from schema_version order by version;
