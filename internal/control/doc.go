// Package control is the control plane's lifecycle controller and placement policy (control-plane plan
// CP-4): domain code, no I/O and no logging. It reaches the world through the ports in ports.go: the
// registries and the placement authority (Postgres, internal/placement/pg via cmd/controld), the agents'
// lifecycle endpoints (internal/control/agenthttp), and object storage (the gc identity's prefix delete).
//
// Flows (option 4 §3; plan §2):
//
//	Create  (flow B)  register "creating" → policy picks a VM → CAS(absent → ACTIVE(vm, 1, 0)) → vm.Claim → "active"
//	Move    (flow C)  "moving" + target → owner.Mover (MOVING, fence, flip at epoch+1, claim) → "active"
//	Suspend           "suspended" → CAS(ACTIVE → MOVING) → fence; Resume is a move from there
//	Drop              "dropping" (the tombstone) → MOVING → fence → delete the prefix → forget →
//	                  delete the placement row → "dropped"
//	Restore (M5)      after an operator attests a VM dead: every key it owns moves elsewhere, and the fence
//	                  of a dead VM is the authority's own row (its last generation is whatever storage holds)
//
// Every flow is resumable: each step reads the authority or the registry and continues from what it finds,
// and ResumeStuck drives unfinished ones from table scans (moving.sql, pending.sql; SQL G-11), never from
// notifications.
//
// Errors: the sentinels in errors.go, owner's sentinels from the authority and the agents, and the ports'
// own errors, wrapped once for module "control".
package control
