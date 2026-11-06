-- cas.sql — Placement.CAS on an existing row; also records the flip when MOVING → ACTIVE on another VM.
-- One statement, one snapshot: the moves row exists iff the flip happened. Under read committed a racing CAS
-- waits for the row, re-checks the where clause on the new version and updates 0 rows (SQL G-9).
-- count 0 ⇒ owner.ErrConflict; 1 ⇒ success. A trigger error (placement_epoch_monotonic) is a caller bug.
-- Params: $1 key; $2..$5 expected owner, epoch, generation, state; $6..$9 next owner, epoch, generation, state.
with flipped as (
     update placement
        set owner_vm = $6, epoch = $7, generation = $8, state = $9, updated_at = now()
      where database_id = $1
        and owner_vm = $2
        and epoch = $3
        and generation = $4
        and state = $5
  returning database_id
),
     recorded as (
     insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation)
          select database_id, $3, $2, $6, $8
            from flipped
           where $5 = 'MOVING'
             and $9 = 'ACTIVE'
             and $2 <> $6
       returning database_id
)
select count(*) as updated
  from flipped;
