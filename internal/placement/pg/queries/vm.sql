-- vm.sql — one VM's registry row. No row ⇒ pg.ErrUnknownVM.
select vm_id,
       endpoint,
       dead_attested_at is not null as dead,
       coalesce(dead_attested_by, '') as dead_attested_by,
       bytes,
       extract(epoch from now() - heartbeat)::float8 as heartbeat_age_seconds
  from vms
 where vm_id = $1;
