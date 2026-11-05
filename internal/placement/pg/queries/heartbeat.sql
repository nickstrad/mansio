-- heartbeat.sql — record a VM's health answer: heartbeat now, the bytes it reported. One HOT update (vms has
-- fillfactor 70 and heartbeat is not indexed). No row ⇒ pg.ErrUnknownVM.
   update vms
      set heartbeat = now(),
          bytes = $2
    where vm_id = $1
returning vm_id;
