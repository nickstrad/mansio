-- owned_by.sql — the placement rows VM $1 owns (VM-loss restore), at most $2, in key order after key $3
-- (keyset pagination). Served by placement_owner_vm.
  select database_id, owner_vm, epoch, generation, state
    from placement
   where owner_vm = $1
     and database_id > $3
order by database_id
   limit $2;
