package tickettest

import (
	"crypto/ed25519"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/ticket"
)

// MemKeys is an in-memory ticket.KeyStore whose keys are derived from Seed and a counter, so a seeded run
// creates the same keys every time. Fail, if set, decides whether a call fails (op: "load", "create",
// "remove"), for fault injection.
type MemKeys struct {
	Seed uint64
	Fail func(op string) bool

	mu   sync.Mutex
	keys map[string]ed25519.PrivateKey
	ages map[string]time.Time
	n    int
}

var _ ticket.KeyStore = (*MemKeys)(nil)

// ErrInjected is what a call Fail chose to fail returns.
var ErrInjected = fmt.Errorf("tickettest: injected key store failure")

func (m *MemKeys) fail(op string) bool { return m.Fail != nil && m.Fail(op) }

// Load returns every key.
func (m *MemKeys) Load() (map[string]ed25519.PrivateKey, []ticket.KeyAge, error) {
	if m.fail("load") {
		return nil, nil, ErrInjected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var ages []ticket.KeyAge
	for _, kid := range slices.Sorted(maps.Keys(m.keys)) {
		ages = append(ages, ticket.KeyAge{KID: kid, Created: m.ages[kid]})
	}
	return maps.Clone(m.keys), ages, nil
}

// Create adds a key created at now: kid "k<n>", seed derived from Seed and n.
func (m *MemKeys) Create(now time.Time) (ticket.KeyAge, ed25519.PrivateKey, error) {
	if m.fail("create") {
		return ticket.KeyAge{}, nil, ErrInjected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys, m.ages = map[string]ed25519.PrivateKey{}, map[string]time.Time{}
	}
	m.n++
	kid := fmt.Sprintf("k%d", m.n)
	priv := NewIssuer(fmt.Sprintf("%d/%s", m.Seed, kid)).Priv
	m.keys[kid], m.ages[kid] = priv, now
	return ticket.KeyAge{KID: kid, Created: now}, priv, nil
}

// Remove deletes kid.
func (m *MemKeys) Remove(kid string) error {
	if m.fail("remove") {
		return ErrInjected
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, kid)
	delete(m.ages, kid)
	return nil
}
