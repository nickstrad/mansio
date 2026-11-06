-- constraints.sql — each constraint of the schema files (0001 …) rejects its off point and accepts its on point
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

-- 0004 api_keys. key_id: on point 16 hex; off points 15 and 17 digits, uppercase. principal: on point 128
-- chars; off points 129 chars, uppercase. secret_sha256: on point 32 bytes; off point 31. perms: on points 1
-- and 3; off points 0 and 4. An unknown tenant fails the foreign key.
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('0123456789abcdef', 'acme', 'p' || repeat('x', 127), sha256('s'), 1);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('0123456789abcde', 'acme', 'p', sha256('s'), 1);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('0123456789abcdef0', 'acme', 'p', sha256('s'), 1);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('0123456789ABCDEF', 'acme', 'p', sha256('s'), 1);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('1123456789abcdef', 'acme', 'p' || repeat('x', 128), sha256('s'), 1);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('1123456789abcdef', 'acme', 'App', sha256('s'), 1);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('1123456789abcdef', 'acme', 'p', substring(sha256('s') from 2), 1);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('1123456789abcdef', 'acme', 'p', sha256('s'), 0);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('1123456789abcdef', 'acme', 'p', sha256('s'), 4);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('1123456789abcdef', 'nobody', 'p', sha256('s'), 3);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms) values ('1123456789abcdef', 'acme', 'p', sha256('s'), 3);

-- What is left.
select database_id, owner_vm, epoch, generation, state from placement order by database_id;
select database_id, from_epoch, from_vm, to_vm, ack_generation from moves order by database_id, from_epoch;
select vm_id, dead_attested_at is null as alive from vms order by vm_id;
select database_id, is_default, state, target_vm from databases order by database_id;
select key_id, tenant_id, length(principal) as principal_len, perms from api_keys order by key_id;
select version from schema_version order by version;

-- 0006 schemas: the fingerprint is SHA-256 of the exact text (on point: the right hash; off points: another
-- text's hash, the hash of the same text indented), its domain (lowercase hex, 64 digits), the size check.
insert into schemas (fingerprint, body) values ('sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a', '{}');
insert into schemas (fingerprint, body) values ('sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a', '{ }');
insert into schemas (fingerprint, body) values ('sha256:' || encode(sha256(convert_to('{ }', 'UTF8')), 'hex'), '{}');
insert into schemas (fingerprint, body) values ('sha256:44136FA355B3678A1146AD16F7E8649E94FB4FC21FE77E8310C060F61CAAFF8A', '{}');
insert into schemas (fingerprint, body) values ('sha256:44136fa355', '{}');
insert into schemas (fingerprint, body) values ('sha256:' || encode(sha256(convert_to('{', 'UTF8')), 'hex'), '{');
insert into schemas (fingerprint, body) values ('sha256:' || encode(sha256(convert_to(repeat('x', 65537), 'UTF8')), 'hex'), repeat('x', 65537));

-- app_schemas: the app is a database name (no colon, lowercase); the fingerprint must be stored; one row per
-- (app, fingerprint).
insert into app_schemas (app, fingerprint) values ('tic-tac-toe', 'sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a');
insert into app_schemas (app, fingerprint) values ('tic-tac-toe', 'sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a');
insert into app_schemas (app, fingerprint) values ('Tic', 'sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a');
insert into app_schemas (app, fingerprint) values ('a:b', 'sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a');
insert into app_schemas (app, fingerprint) values ('chess', 'sha256:' || repeat('0', 64));

-- A registered schema cannot be deleted from under its registration.
delete from schemas where body = '{}';

-- 0007 tenants: slug unique and 1..200 bytes, status one of two, name 1..200 bytes (null allowed: keys-only
-- tenants have none).
insert into tenants (tenant_id, slug, name) values ('00000000-0000-4000-8000-000000000001', 'one', 'One');
insert into tenants (tenant_id, slug, name) values ('00000000-0000-4000-8000-000000000002', 'one', 'Dup');
insert into tenants (tenant_id, slug) values ('00000000-0000-4000-8000-000000000003', '');
insert into tenants (tenant_id, slug) values ('00000000-0000-4000-8000-000000000004', repeat('s', 200));
insert into tenants (tenant_id, slug) values ('00000000-0000-4000-8000-000000000005', repeat('s', 201));
insert into tenants (tenant_id, name) values ('00000000-0000-4000-8000-000000000006', '');
insert into tenants (tenant_id, status) values ('00000000-0000-4000-8000-000000000007', 'paused');
update tenants set status = 'deactivated' where tenant_id = '00000000-0000-4000-8000-000000000004';

-- api_keys: new keys have 16-hex ids, legacy keys uuids (and not the other way round); one legacy row per hash.
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy) values ('00000000-0000-4000-8000-0000000000aa', 'acme', 'p', sha256('legacy'), 3, true);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy) values ('00000000-0000-4000-8000-0000000000ab', 'acme', 'p', sha256('legacy'), 3, true);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy) values ('2123456789abcdef', 'acme', 'p', sha256('legacy'), 3, true);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, legacy) values ('00000000-0000-4000-8000-0000000000ac', 'acme', 'p', sha256('x'), 3, false);
insert into api_keys (key_id, tenant_id, principal, secret_sha256, perms, name) values ('3123456789abcdef', 'acme', 'p', sha256('legacy'), 3, '');

-- auth_users: ids, lowercase unique emails, nonempty names.
insert into auth_users (user_id, email, name) values ('u1', 'ann@example.test', 'Ann');
insert into auth_users (user_id, email, name) values ('u2', 'ann@example.test', 'Ann 2');
insert into auth_users (user_id, email, name) values ('u3', 'Bob@example.test', 'Bob');
insert into auth_users (user_id, email, name) values ('u 4', 'bob@example.test', 'Bob');
insert into auth_users (user_id, email, name) values ('u5', 'bob@example.test', '');

-- auth_passwords: Better Auth's "<32 hex>:<128 hex>", one per user, a user that exists.
insert into auth_passwords (user_id, hash) values ('u1', repeat('a', 32) || ':' || repeat('b', 128));
insert into auth_passwords (user_id, hash) values ('u1', repeat('a', 32) || ':' || repeat('b', 128));
insert into auth_passwords (user_id, hash) values ('nobody', repeat('a', 32) || ':' || repeat('b', 128));
insert into auth_passwords (user_id, hash) values ('u3', repeat('a', 31) || ':' || repeat('b', 128));
insert into auth_passwords (user_id, hash) values ('u3', repeat('A', 32) || ':' || repeat('b', 128));

-- auth_sessions: a 32-byte hash, an expiry after creation.
insert into auth_sessions (token_sha256, user_id, expires_at) values (sha256('t1'), 'u1', now() + interval '1 day');
insert into auth_sessions (token_sha256, user_id, expires_at) values ('\x00', 'u1', now() + interval '1 day');
insert into auth_sessions (token_sha256, user_id, expires_at) values (sha256('t2'), 'u1', now() - interval '1 second');

-- memberships: known user and tenant, a role of two, one row per pair; deleting a user removes its rows.
insert into memberships (user_id, tenant_id) values ('u1', '00000000-0000-4000-8000-000000000001');
insert into memberships (user_id, tenant_id) values ('u1', '00000000-0000-4000-8000-000000000001');
insert into memberships (user_id, tenant_id, role) values ('u3', '00000000-0000-4000-8000-000000000001', 'owner');
insert into memberships (user_id, tenant_id) values ('nobody', '00000000-0000-4000-8000-000000000001');
delete from auth_users where user_id = 'u1';
select count(*) as left_after_user_delete from memberships;
select count(*) as sessions_after_user_delete from auth_sessions;
