package ticket_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/ticket/tickettest"
)

var pol = ticket.RotationPolicy{Rotate: 24 * time.Hour, Activate: 5 * time.Minute, Overlap: 15 * time.Minute}

func TestPlanRotation(t *testing.T) {
	k1 := ticket.KeyAge{KID: "k1", Created: t0}
	k2 := ticket.KeyAge{KID: "k2", Created: t0.Add(pol.Rotate)}
	k2on := k2.Created.Add(pol.Activate) // k2 begins signing
	tests := []struct {
		name string
		keys []ticket.KeyAge
		now  time.Time
		want ticket.Rotation
	}{
		{"no keys", nil, t0, ticket.Rotation{Create: true}},
		{"bootstrap: the only key signs at once", []ticket.KeyAge{k1}, t0, ticket.Rotation{Sign: "k1", Publish: []string{"k1"}}},
		{"just before rotate (off point)", []ticket.KeyAge{k1}, t0.Add(pol.Rotate - 1), ticket.Rotation{Sign: "k1", Publish: []string{"k1"}}},
		{"at rotate (on point)", []ticket.KeyAge{k1}, t0.Add(pol.Rotate), ticket.Rotation{Create: true, Sign: "k1", Publish: []string{"k1"}}},
		{"new key published, not signing", []ticket.KeyAge{k1, k2}, k2on.Add(-1), ticket.Rotation{Sign: "k1", Publish: []string{"k1", "k2"}}},
		{"new key signs at activate", []ticket.KeyAge{k2, k1}, k2on, ticket.Rotation{Sign: "k2", Publish: []string{"k1", "k2"}}},
		{"old key published through overlap (off point)", []ticket.KeyAge{k1, k2}, k2on.Add(pol.Overlap - 1), ticket.Rotation{Sign: "k2", Publish: []string{"k1", "k2"}}},
		{"old key removed after overlap (on point)", []ticket.KeyAge{k1, k2}, k2on.Add(pol.Overlap), ticket.Rotation{Sign: "k2", Publish: []string{"k2"}, Remove: []string{"k1"}}},
		{"two new keys inside activate: the oldest still signs", []ticket.KeyAge{k1, k2, {KID: "k3", Created: k2.Created.Add(time.Minute)}}, k2on.Add(-1),
			ticket.Rotation{Sign: "k1", Publish: []string{"k1", "k2", "k3"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ticket.PlanRotation(tc.keys, tc.now, pol)
			if got.Create != tc.want.Create || got.Sign != tc.want.Sign || !slices.Equal(got.Publish, tc.want.Publish) || !slices.Equal(got.Remove, tc.want.Remove) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A ticket minted at any moment of a rotation verifies at the agent for its whole life, as long as the agent
// fetched the published set at least once per Activate/2.
func TestRotationKeepsEveryLiveTicketVerifiable(t *testing.T) {
	ttl := 10 * time.Minute // Overlap (15 min) >= ttl + skew
	privs := map[string]ed25519.PrivateKey{}
	var ages []ticket.KeyAge
	newKey := func(kid string, at time.Time) {
		seed := sha256.Sum256([]byte(kid))
		privs[kid] = ed25519.NewKeyFromSeed(seed[:])
		ages = append(ages, ticket.KeyAge{KID: kid, Created: at})
	}
	var s ticket.Signer
	ring := ticket.NewKeyRing(nil)
	type live struct {
		tok string
		exp time.Time
	}
	var tickets []live
	step := time.Minute
	for now := t0; now.Before(t0.Add(3 * pol.Rotate)); now = now.Add(step) {
		r := ticket.PlanRotation(ages, now, pol)
		if r.Create {
			newKey(fmt.Sprintf("k%d", len(privs)+int(now.Sub(t0)/time.Minute)), now)
			r = ticket.PlanRotation(ages, now, pol)
		}
		for _, id := range r.Remove {
			delete(privs, id)
		}
		ages = slices.DeleteFunc(ages, func(k ticket.KeyAge) bool { return slices.Contains(r.Remove, k.KID) })
		if err := s.Set(r.Sign, privs, r.Publish); err != nil {
			t.Fatal(err)
		}
		// The agent fetches every step (well under Activate/2).
		b, _ := json.Marshal(s.Public())
		pub, err := ticket.ParsePublicKeys(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		ring.Replace(pub)
		c := iss.Claims("db-1", "vm-1", now, ttl)
		tok, err := s.Mint(c)
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, live{tok, now.Add(ttl)})
		v := ticket.Verifier{Keys: ring, VM: "vm-1", Skew: skew, MaxTTL: time.Hour}
		kept := tickets[:0]
		for _, l := range tickets {
			if !now.Before(l.exp) {
				continue
			}
			if _, err := v.Verify(l.tok, now, want); err != nil {
				t.Fatalf("at %v a live ticket (exp %v) fails: %v", now.Sub(t0), l.exp.Sub(t0), err)
			}
			kept = append(kept, l)
		}
		tickets = kept
	}
	if len(ages) > 3 {
		t.Fatalf("%d keys kept: retired keys are not removed", len(ages))
	}
}

func TestSignerSetRefusesInconsistentRings(t *testing.T) {
	var s ticket.Signer
	if _, err := s.Mint(claims()); err == nil {
		t.Fatal("Mint before Set must fail")
	}
	keys := map[string]ed25519.PrivateKey{"k1": iss.Priv}
	for _, tc := range []struct {
		sign    string
		publish []string
	}{{"k2", []string{"k1"}}, {"k1", nil}, {"k1", []string{"k1", "k9"}}} {
		if err := s.Set(tc.sign, keys, tc.publish); err == nil {
			t.Fatalf("Set(%q, %v) accepted", tc.sign, tc.publish)
		}
	}
	if err := s.Set("k1", keys, []string{"k1"}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.Mint(claims())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier(ticket.NewKeyRing(iss.Keys())).Verify(tok, t0, want); err != nil || s.KID() != "k1" {
		t.Fatalf("%v %s", err, s.KID())
	}
}

// Rotate over a key store: bootstrap signs with the first key at once; a new key is published before it signs;
// the old key is removed only after the overlap; a failed delete keeps the ring installed and is retried.
func TestRotateOverAKeyStore(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	p := ticket.RotationPolicy{Rotate: time.Hour, Activate: time.Minute, Overlap: 10 * time.Minute}
	failRemove := false
	ks := &tickettest.MemKeys{Seed: 1, Fail: func(op string) bool { return op == "remove" && failRemove }}
	var s ticket.Signer
	rotate := func(at time.Duration) (string, []string, error) { return ticket.Rotate(ks, &s, p, t0.Add(at)) }
	if created, _, err := rotate(0); err != nil || created != "k1" || s.KID() != "k1" {
		t.Fatalf("bootstrap: created %q signer %q err %v", created, s.KID(), err)
	}
	if created, _, _ := rotate(time.Hour); created != "k2" || s.KID() != "k1" || len(s.Public().Keys) != 2 {
		t.Fatalf("rotation: created %q, signer %q, published %d; want k2 published, k1 still signing", created, s.KID(), len(s.Public().Keys))
	}
	if _, removed, _ := rotate(time.Hour + time.Minute); s.KID() != "k2" || len(removed) != 0 {
		t.Fatalf("activation: signer %q removed %v", s.KID(), removed)
	}
	failRemove = true
	if _, _, err := rotate(time.Hour + 11*time.Minute); err == nil || s.KID() != "k2" || len(s.Public().Keys) != 1 {
		t.Fatalf("failed delete: err %v signer %q published %d; want the error, k2 alone published", err, s.KID(), len(s.Public().Keys))
	}
	failRemove = false
	if _, removed, err := rotate(time.Hour + 12*time.Minute); err != nil || !slices.Equal(removed, []string{"k1"}) {
		t.Fatalf("retried delete: removed %v err %v", removed, err)
	}
}
