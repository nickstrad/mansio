-- create.sql — Placement.CAS with expect.Epoch == 0 (no row yet). No row returned ⇒ owner.ErrConflict: the
-- key exists already. A racing insert waits for the first to commit, then does nothing.
     insert into placement (database_id, owner_vm, epoch, generation, state)
          values ($1, $2, $3, $4, $5)
     on conflict (database_id) do nothing
  returning epoch;
