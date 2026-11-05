-- live_vms.sql — the edges' routes (controld GET /v1/agents): every VM not attested dead and its query
-- endpoint, at most $1 rows (RI B6). No join: the edge needs no load. A replica's stale answer is safe.
   select vm_id, endpoint
     from vms
    where dead_attested_at is null
 order by vm_id
    limit $1;
