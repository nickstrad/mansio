-- pending.sql — creates and drops to resume (lifecycle controller): databases in 'creating' or 'dropping'
-- for longer than $1, oldest first, at most $2. A table scan, never NOTIFY (SQL G-11). Served by
-- databases_pending.
  select database_id, tenant_id, name, is_default, state, coalesce(target_vm, '') as target_vm, false as inserted
    from databases
   where state in ('creating', 'dropping')
     and updated_at < now() - $1::interval
order by updated_at, database_id
   limit $2;
