-- get.sql — Placement.Get. Primary only: Claim and Reconcile need a linearizable read.
-- No row ⇒ owner.ErrNotFound.
select owner_vm, epoch, generation, state
  from placement
 where database_id = $1;
