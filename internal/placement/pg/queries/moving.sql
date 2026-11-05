-- moving.sql — moves to resume (lifecycle controller): rows MOVING for longer than $1, oldest first, at most
-- $2. A table scan, not NOTIFY: this work must not be lost (SQL G-11). Served by placement_moving.
  select database_id, owner_vm, epoch, generation
    from placement
   where state = 'MOVING'
     and updated_at < now() - $1::interval
order by updated_at, database_id
   limit $2;
