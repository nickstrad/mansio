// Package tickettest mints tickets for tests: an issuer with a deterministic key and valid default claims.
package tickettest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// Issuer signs tickets with one key id.
type Issuer struct {
	KID  string
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// NewIssuer returns an issuer whose key is derived from kid (deterministic, so fuzz corpora stay valid).
func NewIssuer(kid string) Issuer {
	seed := sha256.Sum256([]byte("tickettest " + kid))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return Issuer{KID: kid, Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}
}

// Keys is a key map holding this issuer's key, for ticket.NewKeyRing.
func (i Issuer) Keys() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{i.KID: i.Pub}
}

// Claims are valid claims for db on vm with read and write, issued at now for ttl.
func (i Issuer) Claims(db, vm string, now time.Time, ttl time.Duration) ticket.Claims {
	return ticket.Claims{KID: i.KID, TID: "tenant-1", DBID: db, VM: vm, Epoch: 1, Gen: 1, Sub: "user-1",
		Perms: ticket.Read | ticket.Write, IAT: now.Unix(), EXP: now.Add(ttl).Unix()}
}

// Mint signs c, failing the test on error.
func (i Issuer) Mint(t testing.TB, c ticket.Claims) string {
	t.Helper()
	c.KID = i.KID
	tok, err := ticket.Mint(i.Priv, c)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}
