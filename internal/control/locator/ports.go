package locator

import (
	"context"
	"errors"
	"time"

	"github.com/nickstrad/ts-db-go/internal/ticket"
)

const module = "locator"

// Placement is what a Store knows about one database: its registry state and its placement row.
type Placement struct {
	// Intent is the database's registry state (creating, active, moving, suspended, dropping, dropped), or ""
	// when the registry has no row (a placement row made outside controld, e.g. agentd's -dev create).
	Intent string
	Found  bool // a placement row exists; the fields below are set only then
	VM     string
	// Endpoint is the owner VM's query endpoint (host:port) from the VM registry.
	Endpoint string
	Epoch    uint64
	Gen      uint64
	Moving   bool // the row is MOVING: fenced, a flip or a suspend in progress
}

// StoredKey is an API key's row: never the secret, only its SHA-256.
type StoredKey struct {
	Tenant    string
	Principal string
	Perms     ticket.Perm
	Hash      [32]byte // SHA-256 of the secret
	Revoked   bool
}

// Store reads placement and API keys. Errors other than ErrNotFound are the store's own (wrapped, classified
// by the caller: unavailable, or a bug).
//
//	Locate(key, primary): primary == false may read a replica (stale is fine); true reads the primary
//	APIKey(id):           ErrNotFound when no key has that id
type Store interface {
	Locate(ctx context.Context, key string, primary bool) (Placement, error)
	APIKey(ctx context.Context, id string) (StoredKey, error)
}

// Signer mints tickets with the current signing key (*ticket.Signer).
type Signer interface {
	Mint(c ticket.Claims) (string, error)
}

// Errors. Each is one answer of the resolve API.
var (
	ErrUnauthorized = errors.New("locator: unknown, malformed or revoked api key")
	ErrForbidden    = errors.New("locator: the api key belongs to another tenant")
	ErrNotFound     = errors.New("locator: no such database")
	ErrMoving       = errors.New("locator: database is moving or being created; retry after the hint")
	ErrSuspended    = errors.New("locator: database is suspended")
)

// moving is ErrMoving with a retry hint (front.RetryHint).
type moving struct{ after time.Duration }

func (m moving) Error() string             { return ErrMoving.Error() }
func (m moving) Is(target error) bool      { return target == ErrMoving }
func (m moving) RetryAfter() time.Duration { return m.after }
