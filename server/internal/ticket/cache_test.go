package ticket_test

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/ticket/tickettest"
)

// The cache must give the same answer as the verifier for every rule, on hits as well as misses.
func TestCacheAgreesWithVerifierOnHits(t *testing.T) {
	ring := ticket.NewKeyRing(iss.Keys())
	k := ticket.NewCache(verifier(ring), 8)
	tok := iss.Mint(t, claims())
	if _, err := k.Verify(tok, t0, want); err != nil || k.Len() != 1 {
		t.Fatalf("miss: %v, len %d", err, k.Len())
	}
	exp := time.Unix(claims().EXP, 0)
	tests := []struct {
		name string
		now  time.Time
		want ticket.Want
		err  error
	}{
		{"hit", t0, want, nil},
		{"hit, other db", t0, ticket.Want{DB: "db-2", Perm: ticket.Read}, ticket.ErrDatabase},
		{"hit, missing perm", t0, ticket.Want{DB: "db-1", Perm: ticket.Read | ticket.Write | 4}, ticket.ErrPermission},
		{"hit, not yet valid", t0.Add(-time.Hour), want, ticket.ErrNotYetValid},
		{"hit, expired (evicts)", exp.Add(skew), want, ticket.ErrExpired},
	}
	for _, tc := range tests {
		if _, err := k.Verify(tok, tc.now, tc.want); !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.err)
		}
	}
	if k.Len() != 0 {
		t.Errorf("expired ticket still cached")
	}
}

func TestCacheDropsTicketsOfARemovedOrChangedKey(t *testing.T) {
	ring := ticket.NewKeyRing(iss.Keys())
	k := ticket.NewCache(verifier(ring), 8)
	tok := iss.Mint(t, claims())
	if _, err := k.Verify(tok, t0, want); err != nil {
		t.Fatal(err)
	}
	ring.Replace(nil)
	if _, err := k.Verify(tok, t0, want); !errors.Is(err, ticket.ErrUnknownKey) {
		t.Fatalf("removed key: %v", err)
	}
	impostor := tickettest.NewIssuer("other")
	ring.Replace(map[string]ed25519.PublicKey{"k1": impostor.Pub}) // same kid, different key
	if _, err := k.Verify(tok, t0, want); !errors.Is(err, ticket.ErrSignature) {
		t.Fatalf("changed key: %v", err)
	}
}

func TestCacheBoundAndDisabled(t *testing.T) {
	k := ticket.NewCache(verifier(ticket.NewKeyRing(iss.Keys())), 2)
	for i := range 5 {
		c := claims()
		c.Gen = uint64(i)
		if _, err := k.Verify(iss.Mint(t, c), t0, want); err != nil {
			t.Fatal(err)
		}
		if k.Len() > 2 {
			t.Fatalf("len %d over max 2", k.Len())
		}
	}
	off := ticket.NewCache(verifier(ticket.NewKeyRing(iss.Keys())), 0)
	if _, err := off.Verify(iss.Mint(t, claims()), t0, want); err != nil || off.Len() != 0 {
		t.Fatalf("disabled cache: %v len %d", err, off.Len())
	}
	if _, err := off.Verify("x", t0, want); !errors.Is(err, ticket.ErrMalformed) {
		t.Fatal(err)
	}
}

func BenchmarkCacheHit(b *testing.B) {
	k := ticket.NewCache(verifier(ticket.NewKeyRing(iss.Keys())), 1024)
	tok := iss.Mint(b, claims())
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := k.Verify(tok, t0, want); err != nil {
				b.Fatal(err)
			}
		}
	})
}
