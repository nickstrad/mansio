package edge

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// issuer mints tickets with one key, k1.
type issuer struct {
	priv ed25519.PrivateKey
	ring *ticket.KeyRing
}

func newIssuer(t testing.TB) *issuer {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &issuer{priv: priv, ring: ticket.NewKeyRing(map[string]ed25519.PublicKey{"k1": pub})}
}

// mint returns a ticket for db on vm, valid from now - 1 s for ttl.
func (is *issuer) mint(t testing.TB, vm, db string, now time.Time, ttl time.Duration) string {
	tok, err := ticket.Mint(is.priv, ticket.Claims{KID: "k1", TID: "t1", DBID: db, VM: vm, Epoch: 1, Sub: "u",
		Perms: ticket.Read | ticket.Write, IAT: now.Unix() - 1, EXP: now.Add(ttl).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestGateVerify(t *testing.T) {
	is := newIssuer(t)
	other := newIssuer(t) // same kid, another key: a forged signature
	g := NewGate(is.ring, 5*time.Second, 10*time.Minute, 100)
	good := is.mint(t, "vm-a", "db-1", t0, time.Minute)
	for _, tc := range []struct {
		name  string
		token string
		now   time.Time
		want  error
	}{
		{"valid, any vm", good, t0, nil},
		{"valid for vm-b too", is.mint(t, "vm-b", "db-2", t0, time.Minute), t0, nil},
		{"garbage", "not-a-ticket", t0, ticket.ErrMalformed},
		{"forged", other.mint(t, "vm-a", "db-1", t0, time.Minute), t0, ticket.ErrSignature},
		{"expired: on point exp + skew", good, time.Unix(t0.Add(time.Minute).Unix(), 0).Add(5 * time.Second), ticket.ErrExpired},
		{"not expired: off point", good, time.Unix(t0.Add(time.Minute).Unix(), 0).Add(5*time.Second - time.Nanosecond), nil},
		{"lifetime over max_ticket_ttl", is.mint(t, "vm-a", "db-1", t0, time.Hour), t0, ticket.ErrTTL},
	} {
		_, err := NewGate(is.ring, 5*time.Second, 10*time.Minute, 100).Verify(tc.token, tc.now)
		if !errors.Is(err, tc.want) || (err != nil && !ticket.Invalid(err)) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if c, err := g.Verify(good, t0); err != nil || c.VM != "vm-a" || c.DBID != "db-1" {
		t.Fatalf("claims %+v, %v", c, err)
	}
}

// A verified ticket is a hit until it expires or its key leaves the ring; a miss never returns an error.
func TestGateLookup(t *testing.T) {
	is := newIssuer(t)
	g := NewGate(is.ring, 0, 0, 100)
	tok := is.mint(t, "vm-a", "db-1", t0, time.Minute)
	if _, hit, err := g.Lookup(tok, t0); hit || err != nil {
		t.Fatalf("before verify: hit %v err %v", hit, err)
	}
	g.Verify(tok, t0)
	if c, hit, err := g.Lookup(tok, t0); !hit || err != nil || c.VM != "vm-a" {
		t.Fatalf("after verify: %+v hit %v err %v", c, hit, err)
	}
	if _, hit, err := g.Lookup(tok, t0.Add(-2*time.Second)); !hit || !errors.Is(err, ticket.ErrNotYetValid) {
		t.Fatalf("before iat: hit %v err %v", hit, err)
	}
	if _, hit, err := g.Lookup(tok, t0.Add(2*time.Minute)); !hit || !errors.Is(err, ticket.ErrExpired) || g.Len() != 0 {
		t.Fatalf("expired: hit %v err %v len %d", hit, err, g.Len())
	}
	g.Verify(tok, t0)
	rotated, _, _ := ed25519.GenerateKey(rand.Reader) // k1 now names another key
	is.ring.Replace(map[string]ed25519.PublicKey{"k1": rotated})
	if _, hit, _ := g.Lookup(tok, t0); hit || g.Len() != 0 {
		t.Fatal("a ticket whose key left the ring is still a hit")
	}
}

func TestGateBounded(t *testing.T) {
	is := newIssuer(t)
	g := NewGate(is.ring, 0, 0, 3)
	for i := range 10 {
		if _, err := g.Verify(is.mint(t, "vm-a", "db-"+string(rune('a'+i)), t0, time.Minute), t0); err != nil {
			t.Fatal(err)
		}
		if g.Len() > 3 {
			t.Fatalf("%d entries, max 3", g.Len())
		}
	}
	off := NewGate(is.ring, 0, 0, 0)
	tok := is.mint(t, "vm-a", "db-1", t0, time.Minute)
	if _, err := off.Verify(tok, t0); err != nil || off.Len() != 0 {
		t.Fatalf("cache off: %v, len %d", err, off.Len())
	}
}
