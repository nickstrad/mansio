// Package tenancytest holds an in-memory tenancy.Store with a fault switch, and the Store contract suite that
// the fake and the Postgres adapter both run.
package tenancytest

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/tenancy"
)

// Fake is an in-memory tenancy.Store. Set Fail to make every call return it (e.g. tenancy.ErrUnavailable);
// Calls counts calls. Set Hold (before any call) to make every call wait until it is closed: a slow store.
type Fake struct {
	Hold     chan struct{}
	mu       sync.Mutex
	Fail     error
	Calls    int
	tenants  map[string]tenancy.Tenant
	keys     map[string]fakeKey // by id
	users    map[string]fakeUser
	sessions map[[32]byte]tenancy.Session
	members  map[string]map[string]bool // user → tenants
	clock    time.Time
}

type fakeKey struct {
	tenancy.NewKey
	legacy  bool
	revoked bool
}

type fakeUser struct {
	tenancy.User
	hash string
}

// NewFake returns an empty store.
func NewFake() *Fake {
	return &Fake{tenants: map[string]tenancy.Tenant{}, keys: map[string]fakeKey{}, users: map[string]fakeUser{},
		sessions: map[[32]byte]tenancy.Session{}, members: map[string]map[string]bool{}, clock: time.Unix(1_700_000_000, 0)}
}

// AddLegacyKey stores a legacy key (hash = SHA-256 of the whole key), as the migration does.
func (f *Fake) AddLegacyKey(id, tenant string, hash [32]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[id] = fakeKey{NewKey: tenancy.NewKey{Key: tenancy.Key{ID: id, Tenant: tenant, Name: "legacy"}, Hash: hash}, legacy: true}
}

func (f *Fake) enter() (func(), error) {
	if f.Hold != nil {
		<-f.Hold
	}
	f.mu.Lock()
	f.Calls++
	if f.Fail != nil {
		f.mu.Unlock()
		return nil, f.Fail
	}
	f.clock = f.clock.Add(time.Millisecond) // strictly increasing creation times
	return f.mu.Unlock, nil
}

func (f *Fake) CreateTenant(_ context.Context, t tenancy.Tenant) (tenancy.Tenant, error) {
	done, err := f.enter()
	if err != nil {
		return tenancy.Tenant{}, err
	}
	defer done()
	for _, o := range f.tenants {
		if o.Slug == t.Slug {
			return tenancy.Tenant{}, tenancy.ErrConflict
		}
	}
	t.Created, t.Active = f.clock, true
	f.tenants[t.ID] = t
	return t, nil
}

func (f *Fake) ListTenants(_ context.Context, limit int) ([]tenancy.Tenant, error) {
	done, err := f.enter()
	if err != nil {
		return nil, err
	}
	defer done()
	out := make([]tenancy.Tenant, 0, len(f.tenants))
	for _, t := range f.tenants {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b tenancy.Tenant) int { return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID)) })
	return out[:min(limit, len(out))], nil
}

func (f *Fake) GetTenant(_ context.Context, id string) (tenancy.Tenant, error) {
	done, err := f.enter()
	if err != nil {
		return tenancy.Tenant{}, err
	}
	defer done()
	t, ok := f.tenants[id]
	if !ok {
		return t, tenancy.ErrNotFound
	}
	return t, nil
}

func (f *Fake) DeactivateTenant(_ context.Context, id string) (tenancy.Tenant, error) {
	done, err := f.enter()
	if err != nil {
		return tenancy.Tenant{}, err
	}
	defer done()
	t, ok := f.tenants[id]
	if !ok {
		return t, tenancy.ErrNotFound
	}
	t.Active = false
	f.tenants[id] = t
	for kid, k := range f.keys {
		if k.Tenant == id {
			k.revoked = true
			f.keys[kid] = k
		}
	}
	return t, nil
}

func (f *Fake) CreateKey(_ context.Context, k tenancy.NewKey) (tenancy.Key, error) {
	done, err := f.enter()
	if err != nil {
		return tenancy.Key{}, err
	}
	defer done()
	t, ok := f.tenants[k.Tenant]
	switch _, taken := f.keys[k.ID]; {
	case !ok:
		return tenancy.Key{}, tenancy.ErrNotFound
	case !t.Active:
		return tenancy.Key{}, tenancy.ErrDeactivated
	case taken:
		return tenancy.Key{}, tenancy.ErrConflict
	}
	k.Created = f.clock
	f.keys[k.ID] = fakeKey{NewKey: k}
	return k.Key, nil
}

func (f *Fake) RevokeKey(_ context.Context, tenant, id string) error {
	done, err := f.enter()
	if err != nil {
		return err
	}
	defer done()
	if k, ok := f.keys[id]; ok && k.Tenant == tenant {
		k.revoked = true
		f.keys[id] = k
	}
	return nil
}

func (f *Fake) stored(k fakeKey) tenancy.StoredKey {
	return tenancy.StoredKey{Tenant: k.Tenant, Hash: k.Hash, Revoked: k.revoked, TenantActive: f.tenants[k.Tenant].Active}
}

func (f *Fake) KeyByID(_ context.Context, id string) (tenancy.StoredKey, error) {
	done, err := f.enter()
	if err != nil {
		return tenancy.StoredKey{}, err
	}
	defer done()
	k, ok := f.keys[id]
	if !ok || k.legacy {
		return tenancy.StoredKey{}, tenancy.ErrNotFound
	}
	return f.stored(k), nil
}

func (f *Fake) KeyByLegacyHash(_ context.Context, hash [32]byte) (tenancy.StoredKey, error) {
	done, err := f.enter()
	if err != nil {
		return tenancy.StoredKey{}, err
	}
	defer done()
	for _, k := range f.keys {
		if k.legacy && k.Hash == hash {
			return f.stored(k), nil
		}
	}
	return tenancy.StoredKey{}, tenancy.ErrNotFound
}

func (f *Fake) CreateUser(_ context.Context, u tenancy.User, password string, s tenancy.Session) error {
	done, err := f.enter()
	if err != nil {
		return err
	}
	defer done()
	if _, ok := f.users[u.Email]; ok {
		return tenancy.ErrConflict
	}
	f.users[u.Email] = fakeUser{User: u, hash: password}
	f.sessions[s.Hash] = s
	return nil
}

func (f *Fake) Password(_ context.Context, email string) (tenancy.User, string, error) {
	done, err := f.enter()
	if err != nil {
		return tenancy.User{}, "", err
	}
	defer done()
	u, ok := f.users[email]
	if !ok {
		return tenancy.User{}, "", tenancy.ErrNotFound
	}
	return u.User, u.hash, nil
}

func (f *Fake) CreateSession(_ context.Context, s tenancy.Session) error {
	done, err := f.enter()
	if err != nil {
		return err
	}
	defer done()
	f.sessions[s.Hash] = s
	return nil
}

func (f *Fake) SessionTenant(_ context.Context, hash [32]byte, now time.Time) (string, error) {
	done, err := f.enter()
	if err != nil {
		return "", err
	}
	defer done()
	s, ok := f.sessions[hash]
	if !ok || !now.Before(s.Expires) || len(f.members[s.User]) != 1 {
		return "", tenancy.ErrNotFound
	}
	for t := range f.members[s.User] {
		if f.tenants[t].Active {
			return t, nil
		}
	}
	return "", tenancy.ErrNotFound
}

func (f *Fake) Link(_ context.Context, email, tenant string) error {
	done, err := f.enter()
	if err != nil {
		return err
	}
	defer done()
	u, ok := f.users[email]
	t, tok := f.tenants[tenant]
	switch {
	case !ok || !tok:
		return tenancy.ErrNotFound
	case !t.Active:
		return tenancy.ErrDeactivated
	}
	if f.members[u.ID] == nil {
		f.members[u.ID] = map[string]bool{}
	}
	f.members[u.ID][tenant] = true
	return nil
}
