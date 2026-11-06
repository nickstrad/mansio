-- attest_dead.sql — an operator attests that VM $1 is dead (M5): the precondition for restoring its
-- databases elsewhere without its fence. Idempotent: the first attestation (time and attester) stays.
-- No row ⇒ pg.ErrUnknownVM.
   update vms
      set dead_attested_at = coalesce(dead_attested_at, now()),
          dead_attested_by = coalesce(dead_attested_by, $2)
    where vm_id = $1
returning dead_attested_by;
