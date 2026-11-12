package ticket

import (
	"cmp"
	"crypto/ed25519"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// The issuer's side of key rotation. The issuer holds a ring of Ed25519 signing keys; agents fetch the public half
// (PublicKeyFile) and replace their KeyRing with it. A key goes through
//
//	published, not signing  (age < Activate: agents fetch it before any ticket needs it)
//	signing                 (the newest key at least Activate old)
//	published, retired      (a newer key signs; tickets it signed may still be live)
//	removed                 (Overlap after its successor began signing: every ticket it signed has expired)
//
// PlanRotation is the pure decision; Rotate applies it to a KeyStore (cmd/controld: key files; the simulation:
// tickettest.MemKeys) and installs the result in a Signer.

// RotationPolicy times a key's life. Rotate > 0; Activate ≥ 0; Overlap must be at least the longest ticket
// lifetime plus clock skew, and Activate at least two agent key-fetch periods.
type RotationPolicy struct {
	Rotate   time.Duration // a new key is created when the newest is this old
	Activate time.Duration // a new key signs only after it has been published this long
	Overlap  time.Duration // a retired key stays published this long after its successor began signing
}

// KeyAge is one signing key's id and creation time.
type KeyAge struct {
	KID     string
	Created time.Time
}

// Rotation is what to do now.
type Rotation struct {
	Create  bool     // create a new key (then plan again)
	Sign    string   // the key id that signs; "" only when there are no keys
	Publish []string // key ids to publish, sorted (Sign among them)
	Remove  []string // key ids to delete, sorted
}

// PlanRotation decides which key signs, which are published and which can go.
//
//	post: len(keys) > 0 ⇒ Sign ∈ Publish; Publish ∩ Remove = ∅; Publish ∪ Remove = keys
//	post: Sign is the newest key at least Activate old, or the oldest key when none is (bootstrap: a fresh
//	      ring signs with its only key at once, since no agent has any key yet)
//	post: Create ⇔ no keys, or the newest key is at least Rotate old
func PlanRotation(keys []KeyAge, now time.Time, p RotationPolicy) Rotation {
	if len(keys) == 0 {
		return Rotation{Create: true}
	}
	ks := slices.Clone(keys)
	slices.SortFunc(ks, func(a, b KeyAge) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return cmp.Compare(a.KID, b.KID)
	})
	var r Rotation
	r.Create = now.Sub(ks[len(ks)-1].Created) >= p.Rotate
	sign := 0
	for i, k := range ks {
		if now.Sub(k.Created) >= p.Activate {
			sign = i
		}
	}
	r.Sign = ks[sign].KID
	for i, k := range ks {
		// A key older than the signer retires once the key after it began signing; it is removed Overlap later.
		if i < sign && !now.Before(ks[i+1].Created.Add(p.Activate+p.Overlap)) {
			r.Remove = append(r.Remove, k.KID)
			continue
		}
		r.Publish = append(r.Publish, k.KID)
	}
	slices.Sort(r.Publish)
	slices.Sort(r.Remove)
	assert.That(slices.Contains(r.Publish, r.Sign), "signer %s not published", r.Sign)
	return r
}

// Signer mints tickets with the ring's signing key and serves the published public keys. Set swaps both
// atomically; Mint and Public never lock.
type Signer struct {
	cur atomic.Pointer[signerState]
}

type signerState struct {
	kid    string
	priv   ed25519.PrivateKey
	public PublicKeyFile // built once per Set: agents poll it
}

// Set makes kid's key sign and publishes exactly the public halves of publish (which must contain kid).
func (s *Signer) Set(kid string, keys map[string]ed25519.PrivateKey, publish []string) error {
	pub := map[string]ed25519.PublicKey{}
	for _, id := range publish {
		k, ok := keys[id]
		if !ok || len(k) != ed25519.PrivateKeySize || id == "" || len(id) > MaxKID {
			return fmt.Errorf("ticket: published key %q missing or invalid", id)
		}
		pub[id] = k.Public().(ed25519.PublicKey)
	}
	if _, ok := pub[kid]; !ok {
		return fmt.Errorf("ticket: signing key %q is not published", kid)
	}
	s.cur.Store(&signerState{kid: kid, priv: keys[kid], public: NewPublicKeyFile(pub)})
	return nil
}

// Mint signs c with the current signing key (c.KID is set to it).
func (s *Signer) Mint(c Claims) (string, error) {
	st := s.cur.Load()
	if st == nil {
		return "", fmt.Errorf("ticket: no signing key yet")
	}
	c.KID = st.kid
	return Mint(st.priv, c)
}

// KID is the current signing key id ("" before the first Set).
func (s *Signer) KID() string {
	if st := s.cur.Load(); st != nil {
		return st.kid
	}
	return ""
}

// Public is the published key set, in the agents' key file format, sorted by kid. Do not modify it.
func (s *Signer) Public() PublicKeyFile {
	if st := s.cur.Load(); st != nil {
		return st.public
	}
	return PublicKeyFile{}
}

// KeyStore holds the issuer's signing keys (cmd/controld: a directory of key files).
//
//	Load:   every key with its creation time
//	Create: a new key created at now (stored durably before it returns)
//	Remove: deletes a key; a missing one is not an error
type KeyStore interface {
	Load() (map[string]ed25519.PrivateKey, []KeyAge, error)
	Create(now time.Time) (KeyAge, ed25519.PrivateKey, error)
	Remove(kid string) error
}

// Rotate applies PlanRotation to the keys in s at now: create a key if due, hand the ring to signer, then
// delete the retired keys. It returns what changed. A failure leaves the signer on a ring it can still read:
// the ring is installed before anything is deleted, so a failed delete only keeps a retired key published.
func Rotate(s KeyStore, signer *Signer, p RotationPolicy, now time.Time) (created string, removed []string, err error) {
	privs, ages, err := s.Load()
	if err != nil {
		return "", nil, err
	}
	if privs == nil {
		privs = map[string]ed25519.PrivateKey{}
	}
	r := PlanRotation(ages, now, p)
	if r.Create {
		age, priv, err := s.Create(now)
		if err != nil {
			return "", nil, err
		}
		created = age.KID
		privs[age.KID] = priv
		ages = append(ages, age)
		r = PlanRotation(ages, now, p)
	}
	if err := signer.Set(r.Sign, privs, r.Publish); err != nil {
		return created, nil, errs.Wrap("ticket", err, "cannot install the key ring (a bug); the signer keeps its old ring")
	}
	for _, kid := range r.Remove {
		if err := s.Remove(kid); err != nil {
			return created, removed, err
		}
		removed = append(removed, kid)
	}
	return created, removed, nil
}
