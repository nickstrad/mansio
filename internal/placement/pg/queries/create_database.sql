-- create_database.sql — register database $1 = $2 ':' $3 in state 'creating', creating tenant $2 if new, in
-- one statement. The tenant's first live database becomes its default. Returns the row whether it was
-- inserted now or existed (inserted tells which), so a retried create resumes. A racing first create for the
-- same tenant may fail databases_one_default (pg.ErrInvariant); retrying it is safe.
with tenant as (
     insert into tenants (tenant_id)
          values ($2)
     on conflict (tenant_id) do nothing
),
     inserted as (
     insert into databases (database_id, tenant_id, name, is_default)
          select $1, $2, $3,
                 not exists (select 1
                               from databases
                              where tenant_id = $2
                                and is_default
                                and state <> 'dropped')
     on conflict (database_id) do nothing
       returning database_id, tenant_id, name, is_default, state, coalesce(target_vm, '') as target_vm, true as inserted
)
select database_id, tenant_id, name, is_default, state, target_vm, inserted
  from inserted
 union all
select database_id, tenant_id, name, is_default, state, coalesce(target_vm, ''), false
  from databases
 where database_id = $1
   and not exists (select 1 from inserted);
