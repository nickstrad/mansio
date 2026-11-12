-- invariants.sql — moves rows that break the move invariant, at most $1; empty when the authority is
-- consistent. Every moves row needs its placement row at an epoch above the flip's from_epoch: a
-- flip always bumps the epoch, and epochs never decrease.
   select moves.database_id, moves.from_epoch, placement.epoch
     from moves
          left join placement
                 on placement.database_id = moves.database_id
    where placement.database_id is null
       or placement.epoch <= moves.from_epoch
 order by moves.database_id, moves.from_epoch
    limit $1;
