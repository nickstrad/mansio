-- get_database.sql — one database's registry row. No row ⇒ owner.ErrNotFound.
select database_id, tenant_id, name, is_default, state, coalesce(target_vm, '') as target_vm, false as inserted
  from databases
 where database_id = $1;
