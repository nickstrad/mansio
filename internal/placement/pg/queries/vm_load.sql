-- vm_load.sql — placement policy input: live VMs with their ACTIVE database count, least loaded first, at
-- most $1 rows (RI B6). One set-based query, no per-VM loop, no counter column (SQL G-15, G-17).
   select vms.vm_id, vms.endpoint, count(placement.database_id) as active_dbs
     from vms
          left join placement
                 on placement.owner_vm = vms.vm_id
                and placement.state = 'ACTIVE'
    where vms.dead_attested_at is null
 group by vms.vm_id, vms.endpoint
 order by active_dbs, vms.vm_id
    limit $1;
