-- drop_placement.sql — the last step of a drop: delete key $1's placement row, iff it is still the MOVING row
-- ($2 owner, $3 epoch, $4 generation) the drop fenced, together with its moves rows, in one statement (the
-- foreign key from moves is checked at the statement's end). 0 ⇒ owner.ErrConflict (the row changed or is
-- gone).
with gone as (
     delete from placement
      where database_id = $1
        and owner_vm = $2
        and epoch = $3
        and generation = $4
        and state = 'MOVING'
  returning database_id
),
     moves_gone as (
     delete from moves
      where database_id in (select database_id from gone)
)
select count(*) as deleted
  from gone;
