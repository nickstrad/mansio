-- register_vm.sql — add a VM to the registry (placement.owner_vm references it), or refresh its endpoint
-- and heartbeat. Idempotent. The VM registry (registry.go) owns heartbeats and dead attestation; this is the
-- minimum the placement port needs, since an unregistered owner fails the foreign key.
     insert into vms (vm_id, endpoint)
          values ($1, $2)
     on conflict (vm_id) do update
             set endpoint = excluded.endpoint,
                 heartbeat = now();
