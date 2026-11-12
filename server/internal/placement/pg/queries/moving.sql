-- moving.sql — moves to resume (lifecycle controller): rows MOVING for longer than $1, oldest first, at most
-- $2, each with its database's lifecycle state and move target when the registry knows it (0002): a
-- 'moving' row resumes towards target_vm, 'suspended' stays put, 'dropping' resumes the drop. A table scan,
-- not NOTIFY: this work must not be lost. Served by placement_moving.
   select placement.database_id,
          placement.owner_vm,
          placement.epoch,
          placement.generation,
          coalesce(databases.state, '') as intent,
          coalesce(databases.target_vm, '') as target_vm
     from placement
          left join databases
                 on databases.database_id = placement.database_id
    where placement.state = 'MOVING'
      and placement.updated_at < now() - $1::interval
 order by placement.updated_at, placement.database_id
    limit $2;
