package tenancy

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
)

const module = "tenancy"

// Errors. Store implementations return errors matching these (wrapped); Service passes them on.
var (
	// ErrUnauthenticated: the credential is missing or not valid for the app. Refused (401); as an
	// *AuthError it carries the reason.
	ErrUnauthenticated = errors.New("tenancy: unauthenticated")
	ErrNotFound        = errors.New("tenancy: not found")
	ErrConflict        = errors.New("tenancy: conflict") // a duplicate slug or email
	ErrDeactivated     = errors.New("tenancy: tenant deactivated")
	ErrInvalid         = errors.New("tenancy: invalid input")
	// ErrUnavailable: the store cannot answer now; nothing changed or the outcome is unknown (a write).
	// Retry later (Release It F2).
	ErrUnavailable = errors.New("tenancy: store unavailable")
)

// Reasons an AuthError gives (core/errors.ts AuthError.reason).
const (
	ReasonMissing   = "missing"   // no credential
	ReasonMalformed = "malformed" // not a credential this mode accepts
	ReasonMismatch  = "mismatch"  // unknown, revoked, expired, another app's, or a deactivated tenant's
)

// AuthError is a refused credential. It never carries the credential.
type AuthError struct{ Reason string }

func (e *AuthError) Error() string        { return "tenancy: unauthenticated (" + e.Reason + ")" }
func (e *AuthError) Is(target error) bool { return target == ErrUnauthenticated }

// Mode is what Authenticate accepts.
type Mode uint8

const (
	Off      Mode = iota // each app's own token (AppTokens); one tenant, OffTenant
	APIKeys              // tenant API keys
	Sessions             // tenant API keys and user sessions
)

// ParseMode reads a config value: "off", "api-key" or "session".
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "off":
		return Off, true
	case "api-key":
		return APIKeys, true
	case "session":
		return Sessions, true
	}
	return 0, false
}

// Tenant is a tenants row.
type Tenant struct {
	ID      string // canonical lowercase uuid
	Slug    string
	Name    string
	Active  bool // status 'active' (else 'deactivated')
	Created time.Time
}

// Key is an API key's metadata (never its secret).
type Key struct {
	ID      string // 16 hex digits (new) or a uuid (legacy)
	Tenant  string
	Name    string
	Created time.Time
}

// NewKey is a key row to store: Hash is SHA-256 of the secret.
type NewKey struct {
	Key
	Hash [32]byte
}

// StoredKey is what authenticating a key needs.
type StoredKey struct {
	Tenant       string
	Hash         [32]byte
	Revoked      bool
	TenantActive bool
}

// User is a user's public fields.
type User struct {
	ID       string
	Email    string
	Name     string
	Verified bool
	Created  time.Time
	Updated  time.Time
}

// Session is a session row: the SHA-256 of its token, never the token.
type Session struct {
	Hash    [32]byte
	User    string
	Expires time.Time
}

// Store is the persistence port (internal/placement/pg). Every method takes a ctx with a deadline and returns
// errors matching this package's sentinels; anything else is a bug.
type Store interface {
	// CreateTenant stores t. post: a taken slug ⇒ ErrConflict.
	CreateTenant(ctx context.Context, t Tenant) (Tenant, error)
	// ListTenants returns up to limit tenants, oldest first.
	ListTenants(ctx context.Context, limit int) ([]Tenant, error)
	GetTenant(ctx context.Context, id string) (Tenant, error) // post: no row ⇒ ErrNotFound
	// DeactivateTenant marks id deactivated and revokes its keys, in one transaction. post: no row ⇒ ErrNotFound.
	DeactivateTenant(ctx context.Context, id string) (Tenant, error)
	// CreateKey stores k for an active tenant. post: no tenant ⇒ ErrNotFound; deactivated ⇒ ErrDeactivated;
	// a duplicate id ⇒ ErrConflict.
	CreateKey(ctx context.Context, k NewKey) (Key, error)
	// RevokeKey revokes key id if it belongs to tenant (idempotent; no such key is not an error).
	RevokeKey(ctx context.Context, tenant, id string) error
	KeyByID(ctx context.Context, id string) (StoredKey, error)             // new keys; post: no row ⇒ ErrNotFound
	KeyByLegacyHash(ctx context.Context, hash [32]byte) (StoredKey, error) // legacy keys; post: no row ⇒ ErrNotFound
	// CreateUser stores u, its password hash and its first session, in one transaction. post: a taken email ⇒
	// ErrConflict.
	CreateUser(ctx context.Context, u User, password string, s Session) error
	// Password returns the user and password hash for email. post: no user or no password ⇒ ErrNotFound.
	Password(ctx context.Context, email string) (User, string, error)
	CreateSession(ctx context.Context, s Session) error
	// SessionTenant returns the tenant of the session with this hash: unexpired at now, its user with exactly
	// one membership, that tenant active. post: anything else ⇒ ErrNotFound.
	SessionTenant(ctx context.Context, hash [32]byte, now time.Time) (string, error)
	// Link makes the user with email a member of tenant (idempotent). post: no user or no tenant ⇒
	// ErrNotFound; deactivated ⇒ ErrDeactivated.
	Link(ctx context.Context, email, tenant string) error
}

// Config sets up a Service. Zero fields take defaults.
type Config struct {
	Mode Mode
	// AppTokens are off mode's credentials, by app (each at least 16 bytes).
	AppTokens map[string]string
	OffTenant string // off mode's tenant; default "default"
	// CacheTTL bounds how long a credential's answer is reused (and how long another process may still
	// accept a revoked key); default 1 s. CacheMax bounds the entries; default 10 000.
	CacheTTL time.Duration
	CacheMax int
	// SessionTTL is a new session's lifetime; default 7 days (Better Auth's).
	SessionTTL time.Duration
	// HashSlots bounds concurrent password hashes (each scrypt holds 32 MiB; Release It B2); default 4.
	HashSlots  int
	MaxTenants int       // ListTenants' bound (Release It B6); default 10 000
	Clock      det.Clock // default det.System()
	Rand       io.Reader // secrets and ids; default crypto/rand
}

// Service runs authentication and the admin and sign-in operations over a Store. Safe for concurrent use.
type Service struct {
	store Store
	cfg   Config
	hash  chan struct{} // the password-hash bulkhead

	mu    sync.Mutex
	cache map[[32]byte]cached // by SHA-256 of the credential
	gen   uint64              // bumped by invalidate: an answer read before it is not cached
}

type cached struct {
	p       Principal
	err     error // a refusal (*AuthError), or nil
	expires time.Time
}

// New returns a Service.
func New(store Store, cfg Config) (*Service, error) {
	if cfg.OffTenant == "" {
		cfg.OffTenant = "default"
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = time.Second
	}
	if cfg.CacheMax == 0 {
		cfg.CacheMax = 10_000
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.HashSlots == 0 {
		cfg.HashSlots = 4
	}
	if cfg.MaxTenants == 0 {
		cfg.MaxTenants = 10_000
	}
	cfg.Clock = det.Or(cfg.Clock)
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	var problems []string
	if cfg.Mode > Sessions {
		problems = append(problems, "unknown mode")
	}
	if cfg.Mode == Off && len(cfg.AppTokens) == 0 {
		problems = append(problems, "off mode needs app tokens")
	}
	for app, tok := range cfg.AppTokens {
		if len(tok) < minToken {
			problems = append(problems, fmt.Sprintf("app %q: token shorter than %d bytes", app, minToken))
		}
	}
	if cfg.Mode != Off && store == nil {
		problems = append(problems, "api-key and session modes need a store")
	}
	if cfg.CacheTTL < 0 || cfg.CacheMax < 1 || cfg.SessionTTL < time.Minute || cfg.HashSlots < 1 || cfg.MaxTenants < 1 {
		problems = append(problems, "cache_ttl >= 0, cache_max >= 1, session_ttl >= 1 min, hash_slots >= 1 and max_tenants >= 1 required")
	}
	if len(problems) > 0 {
		return nil, errs.Wrap(module, ErrInvalid, "invalid tenancy config: %s", strings.Join(problems, "; "))
	}
	return &Service{store: store, cfg: cfg, hash: make(chan struct{}, cfg.HashSlots), cache: map[[32]byte]cached{}}, nil
}

// Mode is the configured mode.
func (s *Service) Mode() Mode { return s.cfg.Mode }

// minToken is the shortest app token accepted (a guessable token would let anyone in).
const minToken = 16

// Text limits shared by the admin and sign-up inputs.
const maxText = 200

var (
	uuidRE  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	emailRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

// ValidTenantID reports whether id is a canonical lowercase uuid (the tenant ids this package issues).
func ValidTenantID(id string) bool { return uuidRE.MatchString(id) }

// checkText validates a nonempty admin text field: at most maxText bytes, no control characters.
func checkText(field, v string) error {
	if strings.TrimSpace(v) == "" || len(v) > maxText || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errs.Wrap(module, ErrInvalid, "%s must be 1..%d bytes of text without control characters", field, maxText)
	}
	return nil
}

// newUUID draws a version 4 uuid (tenant ids, as the Deno platform's crypto.randomUUID()).
func (s *Service) newUUID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(s.cfg.Rand, b[:]); err != nil {
		return "", errs.Wrap(module, err, "no randomness; nothing changed")
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// alnum draws n characters from [A-Za-z0-9] without modulo bias (Better Auth's generateId alphabet).
func (s *Service) alnum(n int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 0, n)
	var buf [64]byte
	for len(out) < n {
		if _, err := io.ReadFull(s.cfg.Rand, buf[:]); err != nil {
			return "", errs.Wrap(module, err, "no randomness; nothing changed")
		}
		for _, c := range buf {
			if c < 248 && len(out) < n { // 248 = 4 × 62: rejection sampling keeps every character equally likely
				out = append(out, alphabet[c%62])
			}
		}
	}
	return string(out), nil
}
