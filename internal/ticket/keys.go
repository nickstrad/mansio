package ticket

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync/atomic"
)

// KeyRing is a KeySet whose contents can be replaced while verifiers read it (key rotation). Readers never
// lock: Key is one atomic load of an immutable map.
type KeyRing struct {
	keys atomic.Pointer[map[string]ed25519.PublicKey]
}

// NewKeyRing returns a ring holding a copy of keys.
func NewKeyRing(keys map[string]ed25519.PublicKey) *KeyRing {
	r := &KeyRing{}
	r.Replace(keys)
	return r
}

// Key returns kid's public key.
func (r *KeyRing) Key(kid string) (ed25519.PublicKey, bool) {
	k, ok := (*r.keys.Load())[kid]
	return k, ok
}

// Replace swaps in a copy of keys. Rotate by publishing {old, new}, then, once every old ticket has expired,
// {new}.
func (r *KeyRing) Replace(keys map[string]ed25519.PublicKey) {
	m := maps.Clone(keys)
	if m == nil {
		m = map[string]ed25519.PublicKey{}
	}
	r.keys.Store(&m)
}

// Len is the number of keys held now.
func (r *KeyRing) Len() int { return len(*r.keys.Load()) }

// Equal reports whether the ring holds exactly keys.
func (r *KeyRing) Equal(keys map[string]ed25519.PublicKey) bool {
	return maps.EqualFunc(*r.keys.Load(), keys, func(a, b ed25519.PublicKey) bool { return a.Equal(b) })
}

// KIDs lists the key ids held now (unordered).
func (r *KeyRing) KIDs() []string {
	m := *r.keys.Load()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// PublicKeyFile is the JSON key-set file the agent reads: {"keys": [{"kid": "k1", "public_key": "<base64url>"}]}.
type PublicKeyFile struct {
	Keys []PublicKeyEntry `json:"keys"`
}

// PublicKeyEntry is one key id and its Ed25519 public key (32 bytes, base64url without padding).
type PublicKeyEntry struct {
	KID       string `json:"kid"`
	PublicKey string `json:"public_key"`
}

// NewPublicKeyFile is keys in the key file format, sorted by kid.
func NewPublicKeyFile(keys map[string]ed25519.PublicKey) PublicKeyFile {
	f := PublicKeyFile{Keys: []PublicKeyEntry{}}
	for _, kid := range slices.Sorted(maps.Keys(keys)) {
		f.Keys = append(f.Keys, PublicKeyEntry{KID: kid, PublicKey: EncodeKey(keys[kid])})
	}
	return f
}

// ParsePublicKeys reads a PublicKeyFile. Unknown fields, duplicate or bad key ids and keys of the wrong size are
// errors.
func ParsePublicKeys(r io.Reader) (map[string]ed25519.PublicKey, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var f PublicKeyFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("key set: %w", err)
	}
	if len(f.Keys) == 0 {
		return nil, fmt.Errorf("key set: no keys")
	}
	out := map[string]ed25519.PublicKey{}
	for i, e := range f.Keys {
		if e.KID == "" || len(e.KID) > MaxKID {
			return nil, fmt.Errorf("key set: keys[%d]: kid must be 1..%d bytes", i, MaxKID)
		}
		if _, dup := out[e.KID]; dup {
			return nil, fmt.Errorf("key set: keys[%d]: duplicate kid %q", i, e.KID)
		}
		b, err := b64.DecodeString(e.PublicKey)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("key set: keys[%d] (%s): public_key must be %d bytes of base64url", i, e.KID, ed25519.PublicKeySize)
		}
		out[e.KID] = ed25519.PublicKey(b)
	}
	return out, nil
}

// EncodeKey is base64url (no padding) of a public key or private key seed, as the key files hold them.
func EncodeKey(b []byte) string { return b64.EncodeToString(b) }

// ParsePrivateSeed decodes a base64url Ed25519 seed (32 bytes) into a private key. For tools that mint.
func ParsePrivateSeed(s string) (ed25519.PrivateKey, error) {
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("private key: want %d bytes of base64url seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(b), nil
}
