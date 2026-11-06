package control

import "errors"

var (
	// ErrNoCapacity: no live VM is under the policy's caps.
	ErrNoCapacity = errors.New("control: no vm has capacity")
	// ErrExists: the name belongs to a dropped database (names are never reused).
	ErrExists = errors.New("control: database name was used by a dropped database")
	// ErrState: the operation is not allowed in the database's current lifecycle state.
	ErrState = errors.New("control: not allowed in the database's state")
	// ErrNotDead: a restore needs an operator's attestation that the VM is dead (M5).
	ErrNotDead = errors.New("control: vm not attested dead")
	// ErrNoAgent: the VM has no lifecycle endpoint configured.
	ErrNoAgent = errors.New("control: no agent for vm")
)
