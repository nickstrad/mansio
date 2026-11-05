-- pending.sql — flows to resume (lifecycle controller): databases in 'creating', 'moving' or 'dropping' for
-- longer than $1, oldest first, at most $2. A move whose flip committed but whose claim failed has an ACTIVE
-- placement row, so moving.sql misses it and this finds it. A table scan, never NOTIFY (SQL G-11). Served
-- by databases_pending.
  select database_id, tenant_id, name, is_default, state, coalesce(target_vm, '') as target_vm, false as inserted
    from databases
   where state in ('creating', 'moving', 'dropping')
     and updated_at < now() - $1::interval
order by updated_at, database_id
   limit $2;
