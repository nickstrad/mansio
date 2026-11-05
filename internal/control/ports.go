package control

import (
	"context"
	"time"

	"github.com/nickstrad/ts-db-go/internal/owner"
)

const module = "control"

// Lifecycle states of a database in the registry (0002_registry.sql).
const (
	StateCreating  = "creating"
	StateActive    = "active"
	StateMoving    = "moving"
	StateSuspended = "suspended"
	StateDropping  = "dropping"
	StateDropped   = "dropped"
)

// Database is a registry row.
type Database struct {
	Key     string // tenant ":" name: the placement key and the wire database id
	Tenant  string
	Name    string
	Default bool
	State   string
	Target  string // the move's destination while State is StateMoving
}

// VMLoad is the placement policy's input for one VM not attested dead.
type VMLoad struct {
	VM           string
	ActiveDBs    int64
	Bytes        int64
	HeartbeatAge time.Duration
}

// VM is a VM registry row.
type VM struct {
	ID   string
	Dead bool // an operator attested it dead (M5)
}

// Stuck is a placement row MOVING for a while, with the registry's intent for it.
type Stuck struct {
	Key    string
	Place  owner.Place
	Intent string // the database's state ("" when the registry has no row)
	Target string
}

// Owned is a placement row with its key.
type Owned struct {
	Key   string
	Place owner.Place
}

// Registry is the control plane's registries (Postgres). Errors: owner.ErrNotFound (no such database),
// owner.ErrConflict (a state CAS lost), ErrUnknownVM-like adapter errors otherwise.
//
//	CreateDatabase:   registers tenant:name as "creating" if new; returns the row and whether it was inserted
//	SetDatabaseState: CAS from → to (target only with StateMoving)
//	DropPlacement:    deletes key's placement row iff it is expect (MOVING), with its moves history
//	Load:             VMs not attested dead, with their load (vm_load.sql)
//	Moving, Pending:  table scans of MOVING rows, and of databases creating, moving or dropping
type Registry interface {
	CreateDatabase(ctx context.Context, tenant, name string) (Database, bool, error)
	GetDatabase(ctx context.Context, key string) (Database, error)
	SetDatabaseState(ctx context.Context, key, from, to, target string) error
	DropPlacement(ctx context.Context, key string, expect owner.Place) error
	Load(ctx context.Context, limit int) ([]VMLoad, error)
	VM(ctx context.Context, vm string) (VM, error)
	AttestDead(ctx context.Context, vm, by string) error
	OwnedBy(ctx context.Context, vm, after string, limit int) ([]Owned, error)
	Moving(ctx context.Context, olderThan time.Duration, limit int) ([]Stuck, error)
	Pending(ctx context.Context, olderThan time.Duration, limit int) ([]Database, error)
}

// Agent is one agent's lifecycle endpoints. Errors match owner's sentinels where the agent answered one
// (ErrWrongOwner, ErrMoving, ErrFenced, ErrIllegal, ErrInvalid, ErrUnavailable); anything else means the
// call's outcome is unknown and it may be retried (Fence and Claim are idempotent).
type Agent interface {
	Claim(ctx context.Context, key string) (owner.Local, error)
	Fence(ctx context.Context, key string) (owner.Ack, error)
	Forget(ctx context.Context, key string) error
}

// Objects deletes a database's objects: everything under durable.KeyPrefix(key), and nothing else (object-storage
// guide GC: drop = placement tombstone first, then the prefix). Idempotent.
type Objects interface {
	DeletePrefix(ctx context.Context, key string) (int, error)
}
