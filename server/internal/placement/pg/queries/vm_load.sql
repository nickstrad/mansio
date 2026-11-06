-- vm_load.sql — placement policy input: VMs not attested dead, with their ACTIVE database count, the bytes
-- they last reported and the age of their last heartbeat, least loaded first, at most $1 rows (RI B6). One
-- set-based query, no per-VM loop, no counter column (SQL G-15, G-17). The policy (internal/control) decides
-- which are alive and under the cap.
   select vms.vm_id,
          vms.endpoint,
          count(placement.database_id) as active_dbs,
          vms.bytes,
          extract(epoch from now() - vms.heartbeat)::float8 as heartbeat_age_seconds
     from vms
          left join placement
                 on placement.owner_vm = vms.vm_id
                and placement.state = 'ACTIVE'
    where vms.dead_attested_at is null
 group by vms.vm_id, vms.endpoint, vms.bytes, vms.heartbeat
 order by active_dbs, vms.bytes, vms.vm_id
    limit $1;
