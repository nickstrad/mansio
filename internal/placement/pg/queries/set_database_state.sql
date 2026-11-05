-- set_database_state.sql — CAS on a database's lifecycle state: $1 key, $2 expected state, $3 next state,
-- $4 next target VM ('' = none). 0 rows ⇒ owner.ErrConflict (the state was not $2). The
-- databases_target_iff_moving check refuses a target outside 'moving' (pg.ErrInvariant, a caller bug).
   update databases
      set state = $3,
          target_vm = nullif($4, ''),
          updated_at = now()
    where database_id = $1
      and state = $2
returning database_id;
