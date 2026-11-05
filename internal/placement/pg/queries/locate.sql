-- locate.sql — the locator's read: one database's registry state, placement row and owner endpoint. Any part
-- may be missing (left joins from the key itself), so "no such database", "being created" and "placed" are
-- one round trip. Run on a replica or the primary (the adapter's choice); a stale answer is safe (option 4
-- §3.3).
select coalesce(d.state, '') as intent,
       p.owner_vm,
       v.endpoint,
       p.epoch,
       p.generation,
       p.state
  from (values ($1::text)) as k (database_id)
       left join databases as d
              on d.database_id = k.database_id
       left join placement as p
              on p.database_id = k.database_id
       left join vms as v
              on v.vm_id = p.owner_vm;
