package ticket_test

import (
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/ticket"
)

// FuzzVerify: arbitrary input never panics, and is accepted only if it is exactly the ticket the issuer would
// mint for the claims it decodes to (Ed25519 is deterministic, decoding is canonical), i.e. nothing forged or
// re-encoded verifies.
func FuzzVerify(f *testing.F) {
	v := ticket.Verifier{Keys: ticket.NewKeyRing(iss.Keys()), VM: "vm-1", Skew: skew}
	c := claims()
	f.Add(iss.Mint(f, c))
	c.Perms = ticket.Write
	f.Add(iss.Mint(f, c))
	c.Epoch, c.Gen = 1<<63, 300
	f.Add(iss.Mint(f, c))
	f.Add("")
	f.Add("AQJrMQ")
	cache := ticket.NewCache(v, 64)
	f.Fuzz(func(t *testing.T, tok string) {
		w := ticket.Want{DB: "db-1", Perm: ticket.Read}
		got, err := v.Verify(tok, t0, w)
		ticket.Parse(tok) // total: must not panic either
		for range 2 {     // miss, then hit: the cache agrees with the verifier
			if cg, cerr := cache.Verify(tok, t0, w); cerr != err || cg != got {
				t.Fatalf("cache: %+v %v, verifier: %+v %v", cg, cerr, got, err)
			}
		}
		if err != nil {
			return
		}
		again, merr := ticket.Mint(iss.Priv, got)
		if merr != nil || again != tok {
			t.Fatalf("accepted a ticket the issuer did not mint: %q (re-mint %q, %v)", tok, again, merr)
		}
		if got.DBID != "db-1" || got.VM != "vm-1" || !got.Perms.Has(ticket.Read) || !t0.Before(time.Unix(got.EXP, 0).Add(skew)) {
			t.Fatalf("accepted claims that do not satisfy the request: %+v", got)
		}
	})
}
