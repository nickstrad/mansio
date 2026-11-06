package owner

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickstrad/ts-db-go/internal/durable"
)

const module = "owner"

// Admission sentinels. Admit returns them bare (no wrapping, no stack capture) because rejections are on the
// hot path; the front maps each to a wire code. Lifecycle calls wrap them with errs.Wrap.
var (
	// ErrWrongOwner: this process does not hold the key at the request's epoch (absent here, or the epoch
	// is older or newer than the local ACTIVE epoch). The client re-resolves.
	ErrWrongOwner = errors.New("owner: wrong owner")
	// ErrMoving: a fence is in progress (or its write-back failed and waits for a retry). The client
	// re-resolves after a backoff.
	ErrMoving = errors.New("owner: moving")
	// ErrFenced: durably fenced at this epoch (handed off, or superseded by a higher epoch).
	ErrFenced = errors.New("owner: fenced")
	// ErrUnavailable: the process restarted and has not reconciled this key with the authority, or the
	// authority could not be reached or contradicted local state. Nothing is admitted; retry after backoff.
	ErrUnavailable = errors.New("owner: unavailable")
)

// Other sentinels.
var (
	// ErrSuperseded: a publish found a higher epoch in storage (contract 1 ErrStale after this process
	// stamped its epoch). The key is now fenced durably; the process must not serve it.
	ErrSuperseded = errors.New("owner: superseded by a higher epoch")
	// ErrIllegal: a transition the table does not allow (see Step).
	ErrIllegal = errors.New("owner: illegal transition")
	// ErrInvalid: bad input (empty or too long key, epoch 0, malformed record).
	ErrInvalid = errors.New("owner: invalid input")
	// ErrStale is what a WriteBack returns (wrapped is fine) when contract 1's conditional publish fails:
	// the stored manifest changed since it was read, or its epoch is greater than the caller's.
	ErrStale = errors.New("owner: stale publish")
	// ErrNoManifest is what WriteBack.Head returns when the key has no manifest yet.
	ErrNoManifest = errors.New("owner: no manifest")
	// ErrNotFound is what Placement.Get returns for a key the authority does not know.
	ErrNotFound = errors.New("owner: placement not found")
	// ErrConflict is what Placement.CAS returns when the current row is not the expected one.
	ErrConflict = errors.New("owner: placement conflict")
)

// MaxKeyLen is durable.MaxKeyLen: one key limit for ownership and storage. It also keeps a record's hex file
// name (2 bytes per key byte, plus ".rec") within 255 bytes.
const MaxKeyLen = durable.MaxKeyLen

// ValidKey reports whether key may be used: 1 to MaxKeyLen bytes.
func ValidKey(key string) bool { return len(key) >= 1 && len(key) <= MaxKeyLen }

// PlaceState is the authority's state for a key.
type PlaceState uint8

const (
	PlaceActive PlaceState = iota + 1
	PlaceMoving
)

func (s PlaceState) String() string {
	switch s {
	case PlaceActive:
		return "ACTIVE"
	case PlaceMoving:
		return "MOVING"
	}
	return "?"
}

// Place is the authority's row for a key (option 4 §1: database_id, owner_vm, epoch, generation, state).
type Place struct {
	Owner string
	Epoch Epoch
	Gen   Generation
	State PlaceState
}

// Placement is the authority that hands out epochs. The control plane implements it (Postgres placement row);
// ownertest.Placement is the fake.
//
//	Get:  linearizable: it returns the latest committed row (read the primary, not a replica). A missing key
//	      matches ErrNotFound. The locator may serve stale answers to clients; this port must not.
//	CAS:  sets the row to next iff it currently equals expect (all four fields); otherwise ErrConflict and
//	      nothing changes. expect.Epoch == 0 means "no row" (create). The authority keeps two invariants:
//	      epochs never decrease, and the owner changes only together with an epoch increase.
type Placement interface {
	Get(ctx context.Context, key string) (Place, error)
	CAS(ctx context.Context, key string, expect, next Place) error
}

// Manifest is the part of contract 1's manifest that ownership needs.
type Manifest struct {
	Gen   Generation
	Epoch Epoch
}

// WriteBack is the durable copy (contract 1); writeback implements it. Every method is conditional on the
// stored manifest, so a lower epoch never overwrites a higher one.
//
//	Publish: upload the local copy as a new generation and publish the manifest at epoch, iff the stored
//	         manifest is unchanged since read and its epoch ≤ epoch. Returns the published generation.
//	Head:    read the stored manifest; ErrNoManifest if none.
//	Stamp:   re-publish the stored manifest unchanged except epoch := epoch (no upload), iff the stored
//	         generation is still gen and the stored epoch ≤ epoch. No manifest and gen == 0 creates one for an
//	         empty database.
//	errors:  a failed condition matches ErrStale. Any other error means nothing is known to have changed.
//	         An implementation resolves its own ambiguous writes (a lost reply, or a 412 its own re-sent PUT
//	         caused) by re-reading before it answers (object-storage guide R3): ErrStale means the re-read
//	         shows another writer, never our own write.
type WriteBack interface {
	Publish(ctx context.Context, key string, epoch Epoch) (Generation, error)
	Head(ctx context.Context, key string) (Manifest, error)
	Stamp(ctx context.Context, key string, epoch Epoch, gen Generation) error
}

// Record is one key's durable local state. Only Active and Fenced are ever stored.
type Record struct {
	Key        string
	State      State
	Epoch      Epoch
	Gen        Generation
	Superseded bool // only with Fenced
}

// Validate checks a record at the edge of a store: a valid key, only Active or Fenced, epoch ≥ 1, and
// Superseded only with Fenced. Errors match ErrInvalid.
func (r Record) Validate() error {
	switch {
	case !ValidKey(r.Key):
		return fmt.Errorf("%w: key must be 1..%d bytes", ErrInvalid, MaxKeyLen)
	case r.State != Active && r.State != Fenced:
		return fmt.Errorf("%w: only active and fenced are stored, got %s", ErrInvalid, r.State)
	case r.Epoch == 0:
		return fmt.Errorf("%w: epoch 0", ErrInvalid)
	case r.Superseded && r.State != Fenced:
		return fmt.Errorf("%w: superseded is only stored with fenced", ErrInvalid)
	}
	return nil
}

// LocalStore keeps Records on local disk (FileStore) or in memory (ownertest.MemStore).
//
//	Put:    atomic and durable: after it returns nil, Load returns the record even after a crash; if it
//	        fails or the process crashes during it, Load returns either the old record or the new one.
//	Delete: durable; deleting a missing key is not an error.
//	Load:   every stored record, any order. A record that cannot be read is an error (fail closed), never
//	        silently skipped.
//	pre:    callers serialize Put/Delete per key (Table does); different keys may run concurrently.
type LocalStore interface {
	Put(ctx context.Context, r Record) error
	Delete(ctx context.Context, key string) error
	Load(ctx context.Context) ([]Record, error)
}
