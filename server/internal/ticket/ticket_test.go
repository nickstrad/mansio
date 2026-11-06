package ticket_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/ticket"
	"github.com/nickstrad/mansio/server/internal/ticket/tickettest"
)

var (
	iss = tickettest.NewIssuer("k1")
	t0  = time.Unix(1_800_000_000, 0)
)

const skew = 5 * time.Second

func verifier(keys ticket.KeySet) ticket.Verifier {
	return ticket.Verifier{Keys: keys, VM: "vm-1", Skew: skew, MaxTTL: 10 * time.Minute}
}

func claims() ticket.Claims { return iss.Claims("db-1", "vm-1", t0, time.Minute) }

var want = ticket.Want{DB: "db-1", Perm: ticket.Read}

func TestRoundTrip(t *testing.T) {
	c := claims()
	tok := iss.Mint(t, c)
	got, err := verifier(ticket.NewKeyRing(iss.Keys())).Verify(tok, t0, want)
	if err != nil || got != c {
		t.Fatalf("got %+v %v, want %+v", got, err, c)
	}
	if p, err := ticket.Parse(tok); err != nil || p != c {
		t.Fatalf("Parse: %+v %v", p, err)
	}
	if len(tok) > 300 {
		t.Fatalf("ticket is %d bytes; the encoding should stay compact", len(tok))
	}
}

// Time boundaries: expired ⇔ now >= exp + skew; not yet valid ⇔ now < iat - skew; lifetime cap.
func TestVerifyTimeBoundaries(t *testing.T) {
	c := claims() // iat = t0, exp = t0 + 60 s
	exp := time.Unix(c.EXP, 0)
	iat := time.Unix(c.IAT, 0)
	tests := []struct {
		name string
		now  time.Time
		err  error
	}{
		{"at iat", iat, nil},
		{"before exp", exp.Add(-time.Nanosecond), nil},
		{"at exp, inside skew", exp, nil},
		{"last instant inside skew (on point)", exp.Add(skew - time.Nanosecond), nil},
		{"exp + skew (off point)", exp.Add(skew), ticket.ErrExpired},
		{"long after", exp.Add(time.Hour), ticket.ErrExpired},
		{"iat - skew (on point)", iat.Add(-skew), nil},
		{"just before iat - skew (off point)", iat.Add(-skew - time.Nanosecond), ticket.ErrNotYetValid},
	}
	v := verifier(ticket.NewKeyRing(iss.Keys()))
	tok := iss.Mint(t, c)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(tok, tc.now, want); !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
		})
	}
}

func TestVerifyLifetimeCap(t *testing.T) {
	v := verifier(ticket.NewKeyRing(iss.Keys())) // MaxTTL 10 min
	for _, tc := range []struct {
		ttl time.Duration
		err error
	}{{10 * time.Minute, nil}, {10*time.Minute + time.Second, ticket.ErrTTL}} {
		c := claims()
		c.EXP = c.IAT + int64(tc.ttl/time.Second)
		if _, err := v.Verify(iss.Mint(t, c), t0, want); !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
			t.Errorf("ttl %v: err %v, want %v", tc.ttl, err, tc.err)
		}
	}
	v.MaxTTL = 0 // no cap
	c := claims()
	c.EXP = c.IAT + 86400*365
	if _, err := v.Verify(iss.Mint(t, c), t0, want); err != nil {
		t.Errorf("no cap: %v", err)
	}
}

func TestVerifyPermsAudienceDatabase(t *testing.T) {
	v := verifier(ticket.NewKeyRing(iss.Keys()))
	tests := []struct {
		name  string
		perms ticket.Perm
		vm    string
		want  ticket.Want
		err   error
	}{
		{"read ticket, read", ticket.Read, "vm-1", ticket.Want{"db-1", ticket.Read}, nil},
		{"read ticket, write", ticket.Read, "vm-1", ticket.Want{"db-1", ticket.Write}, ticket.ErrPermission},
		{"write ticket, read (write does not imply read)", ticket.Write, "vm-1", ticket.Want{"db-1", ticket.Read}, ticket.ErrPermission},
		{"write ticket, write", ticket.Write, "vm-1", ticket.Want{"db-1", ticket.Write}, nil},
		{"read|write, write", ticket.Read | ticket.Write, "vm-1", ticket.Want{"db-1", ticket.Write}, nil},
		{"read|write, both", ticket.Read | ticket.Write, "vm-1", ticket.Want{"db-1", ticket.Read | ticket.Write}, nil},
		{"no permission asked is a caller bug, refused", ticket.Read, "vm-1", ticket.Want{"db-1", 0}, ticket.ErrPermission},
		{"other vm", ticket.Read, "vm-2", ticket.Want{"db-1", ticket.Read}, ticket.ErrAudience},
		{"other db", ticket.Read, "vm-1", ticket.Want{"db-2", ticket.Read}, ticket.ErrDatabase},
		{"db prefix is not a match", ticket.Read, "vm-1", ticket.Want{"db-11", ticket.Read}, ticket.ErrDatabase},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := claims()
			c.Perms, c.VM = tc.perms, tc.vm
			if _, err := v.Verify(iss.Mint(t, c), t0, tc.want); !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
		})
	}
}

// signRaw signs arbitrary body bytes the way Mint does, to build tickets Mint refuses to make.
func signRaw(priv ed25519.PrivateKey, body []byte) string {
	sig := ed25519.Sign(priv, append([]byte(ticket.Context), body...))
	return base64.RawURLEncoding.EncodeToString(append(body, sig...))
}

func TestVerifyRejectsBadEncodings(t *testing.T) {
	v := verifier(ticket.NewKeyRing(iss.Keys()))
	good := iss.Mint(t, claims())
	raw, _ := base64.RawURLEncoding.DecodeString(good)
	body := raw[:len(raw)-64]
	flip := func(i int) string {
		b := bytes.Clone(raw)
		b[i] ^= 1
		return base64.RawURLEncoding.EncodeToString(b)
	}
	other := tickettest.NewIssuer("k2")
	tests := []struct {
		name string
		tok  string
		err  error
	}{
		{"empty", "", ticket.ErrMalformed},
		{"not base64", "!!!!", ticket.ErrMalformed},
		{"padded base64", good + "==", ticket.ErrMalformed},
		{"CR inside (decoder would skip it; found by FuzzVerify)", good[:20] + "\r" + good[20:], ticket.ErrMalformed},
		{"LF at the end", good + "\n", ticket.ErrMalformed},
		{"space inside", good[:20] + " " + good[20:], ticket.ErrMalformed},
		{"std alphabet", strings.NewReplacer("-", "+", "_", "/").Replace(good) + "+", ticket.ErrMalformed},
		{"too long", strings.Repeat("A", ticket.MaxTicket+1), ticket.ErrMalformed},
		{"shorter than header + signature", base64.RawURLEncoding.EncodeToString(make([]byte, 65)), ticket.ErrMalformed},
		{"version 2", base64.RawURLEncoding.EncodeToString(append([]byte{2}, raw[1:]...)), ticket.ErrVersion},
		{"kid length 0", base64.RawURLEncoding.EncodeToString(append([]byte{1, 0}, raw[2:]...)), ticket.ErrMalformed},
		{"kid length past the end", base64.RawURLEncoding.EncodeToString(append([]byte{1, 255}, raw[2:]...)), ticket.ErrMalformed},
		{"unknown kid", other.Mint(t, claims()), ticket.ErrUnknownKey},
		{"flipped claim byte", flip(10), ticket.ErrSignature},
		{"flipped signature byte", flip(len(raw) - 1), ticket.ErrSignature},
		{"signed by another key under k1", signRaw(other.Priv, body), ticket.ErrSignature},
		{"trailing byte, signed", signRaw(iss.Priv, append(bytes.Clone(body), 0)), ticket.ErrMalformed},
		{"truncated claims, signed", signRaw(iss.Priv, body[:len(body)-3]), ticket.ErrMalformed},
	}
	// A non-minimal varint for the epoch (0x81 0x00 = 1): valid signature, not canonical.
	c := claims()
	nonMin := []byte{1, byte(len(c.KID))}
	nonMin = append(nonMin, c.KID...)
	for _, s := range []string{c.TID, c.DBID, c.VM} {
		nonMin = append(append(nonMin, byte(len(s))), s...)
	}
	nonMin = append(nonMin, 0x81, 0x00, 1, byte(len(c.Sub)))
	nonMin = append(append(nonMin, c.Sub...), byte(c.Perms))
	tests = append(tests, struct {
		name string
		tok  string
		err  error
	}{"non-minimal varint, signed", signRaw(iss.Priv, append(nonMin, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01)), ticket.ErrMalformed})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(tc.tok, t0, want); !errors.Is(err, tc.err) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
		})
	}
}

// A signed ticket whose claims no issuer should produce is refused with ErrClaims.
func TestVerifyRejectsSignedInvalidClaims(t *testing.T) {
	v := verifier(ticket.NewKeyRing(iss.Keys()))
	for name, mut := range map[string]func(*ticket.Claims){
		"unknown perm bit": func(c *ticket.Claims) { c.Perms = 4 | ticket.Read },
		"exp == iat":       func(c *ticket.Claims) { c.EXP = c.IAT },
	} {
		c := claims()
		mut(&c)
		// Build the body by minting valid claims with the same lengths, then patching the signed bytes.
		good := claims()
		raw, _ := base64.RawURLEncoding.DecodeString(iss.Mint(t, good))
		body := bytes.Clone(raw[:len(raw)-64])
		if c.Perms != good.Perms {
			i := bytes.LastIndexByte(body[:len(body)-10], byte(good.Perms))
			body[i] = byte(c.Perms)
		} else {
			// iat and exp are the last two varints (5 bytes each at t0): copy iat over exp.
			copy(body[len(body)-5:], body[len(body)-10:len(body)-5])
		}
		if _, err := v.Verify(signRaw(iss.Priv, body), t0, want); !errors.Is(err, ticket.ErrClaims) {
			t.Errorf("%s: err %v, want ErrClaims", name, err)
		}
	}
}

func TestMintValidatesClaims(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	tests := []struct {
		name string
		mut  func(*ticket.Claims)
		ok   bool
	}{
		{"valid", func(*ticket.Claims) {}, true},
		{"kid at max (on)", func(c *ticket.Claims) { c.KID = long(ticket.MaxKID) }, true},
		{"kid over max (off)", func(c *ticket.Claims) { c.KID = long(ticket.MaxKID + 1) }, false},
		{"empty kid", func(c *ticket.Claims) { c.KID = "" }, false},
		{"empty tid", func(c *ticket.Claims) { c.TID = "" }, false},
		{"tid at max", func(c *ticket.Claims) { c.TID = long(ticket.MaxTID) }, true},
		{"tid over max", func(c *ticket.Claims) { c.TID = long(ticket.MaxTID + 1) }, false},
		{"dbid at max", func(c *ticket.Claims) { c.DBID = long(ticket.MaxDBID) }, true},
		{"dbid over max", func(c *ticket.Claims) { c.DBID = long(ticket.MaxDBID + 1) }, false},
		{"vm over max", func(c *ticket.Claims) { c.VM = long(ticket.MaxVM + 1) }, false},
		{"empty sub", func(c *ticket.Claims) { c.Sub = "" }, false},
		{"sub at max", func(c *ticket.Claims) { c.Sub = long(ticket.MaxSub) }, true},
		{"sub over max", func(c *ticket.Claims) { c.Sub = long(ticket.MaxSub + 1) }, false},
		{"no perms", func(c *ticket.Claims) { c.Perms = 0 }, false},
		{"unknown perm", func(c *ticket.Claims) { c.Perms = 4 }, false},
		{"negative iat", func(c *ticket.Claims) { c.IAT = -1 }, false},
		{"iat 0 (on)", func(c *ticket.Claims) { c.IAT, c.EXP = 0, 1 }, true},
		{"exp = iat (off)", func(c *ticket.Claims) { c.EXP = c.IAT }, false},
		{"exp = iat + 1 (on)", func(c *ticket.Claims) { c.EXP = c.IAT + 1 }, true},
		{"exp at MaxUnix (on)", func(c *ticket.Claims) { c.EXP = ticket.MaxUnix }, true},
		{"exp past MaxUnix (off)", func(c *ticket.Claims) { c.EXP = ticket.MaxUnix + 1 }, false},
		{"max epoch and gen", func(c *ticket.Claims) { c.Epoch, c.Gen = ^uint64(0), ^uint64(0) }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := claims()
			tc.mut(&c)
			tok, err := ticket.Mint(iss.Priv, c)
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				if !errors.Is(err, ticket.ErrClaims) {
					t.Fatalf("err %v is not ErrClaims", err)
				}
				return
			}
			if len(tok) > ticket.MaxTicket {
				t.Fatalf("max-size claims make a %d-byte ticket, over MaxTicket", len(tok))
			}
			if p, err := ticket.Parse(tok); err != nil || p != c { // Mint's postcondition
				t.Fatalf("Parse: %+v %v", p, err)
			}
		})
	}
	if _, err := ticket.Mint(iss.Priv[:10], claims()); err == nil {
		t.Fatal("short private key accepted")
	}
}

// Rotation: the ring holds {k1, k2} while both are live, then {k2}; verifiers racing the swaps never see a
// torn set (run with -race).
func TestKeyRingRotationMidFlight(t *testing.T) {
	k2 := tickettest.NewIssuer("k2")
	ring := ticket.NewKeyRing(iss.Keys())
	v := verifier(ring)
	old, neu := iss.Mint(t, claims()), k2.Mint(t, claims())
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := v.Verify(old, t0, want)
				if err != nil && !errors.Is(err, ticket.ErrUnknownKey) {
					t.Errorf("old ticket: %v", err)
				}
			}
		})
	}
	both := map[string]ed25519.PublicKey{"k1": iss.Pub, "k2": k2.Pub}
	for range 100 {
		ring.Replace(both)
		ring.Replace(k2.Keys())
	}
	ring.Replace(both)
	if _, err := v.Verify(old, t0, want); err != nil {
		t.Errorf("overlap: old ticket refused: %v", err)
	}
	if _, err := v.Verify(neu, t0, want); err != nil {
		t.Errorf("overlap: new ticket refused: %v", err)
	}
	ring.Replace(k2.Keys())
	if _, err := v.Verify(old, t0, want); !errors.Is(err, ticket.ErrUnknownKey) {
		t.Errorf("after rotation: old ticket %v", err)
	}
	close(stop)
	wg.Wait()
	if len(ring.KIDs()) != 1 {
		t.Errorf("KIDs %v", ring.KIDs())
	}
	both["k3"] = nil // Replace copied: later changes to the caller's map do not leak in
	if _, ok := ring.Key("k3"); ok {
		t.Error("ring aliases the caller's map")
	}
	ring.Replace(nil)
	if _, ok := ring.Key("k2"); ok {
		t.Error("Replace(nil) kept keys")
	}
}

func TestVerifyRejectsAWrongSizeKey(t *testing.T) {
	v := verifier(ticket.NewKeyRing(map[string]ed25519.PublicKey{"k1": iss.Pub[:31]}))
	if _, err := v.Verify(iss.Mint(t, claims()), t0, want); !errors.Is(err, ticket.ErrUnknownKey) {
		t.Fatalf("err %v", err)
	}
}

func TestParsePublicKeys(t *testing.T) {
	pub := ticket.EncodeKey(iss.Pub)
	tests := []struct {
		name, body string
		ok         bool
	}{
		{"one key", `{"keys":[{"kid":"k1","public_key":"` + pub + `"}]}`, true},
		{"two keys", `{"keys":[{"kid":"k1","public_key":"` + pub + `"},{"kid":"k2","public_key":"` + pub + `"}]}`, true},
		{"no keys", `{"keys":[]}`, false},
		{"duplicate kid", `{"keys":[{"kid":"k1","public_key":"` + pub + `"},{"kid":"k1","public_key":"` + pub + `"}]}`, false},
		{"empty kid", `{"keys":[{"kid":"","public_key":"` + pub + `"}]}`, false},
		{"short key", `{"keys":[{"kid":"k1","public_key":"` + pub[:40] + `"}]}`, false},
		{"unknown field", `{"keys":[],"extra":1}`, false},
		{"not json", `nope`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ticket.ParsePublicKeys(strings.NewReader(tc.body))
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && !m["k1"].Equal(iss.Pub) {
				t.Fatalf("keys %v", m)
			}
		})
	}
	seed := ticket.EncodeKey(iss.Priv.Seed())
	if k, err := ticket.ParsePrivateSeed(seed); err != nil || !k.Equal(iss.Priv) {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ticket.ParsePrivateSeed(seed[:10]); err == nil {
		t.Fatal("short seed accepted")
	}
}

func TestPermString(t *testing.T) {
	for p, s := range map[ticket.Perm]string{ticket.Read: "read", ticket.Write: "write", ticket.Read | ticket.Write: "read|write", 4: "perm(4)"} {
		if p.String() != s {
			t.Errorf("%d: %q", p, p.String())
		}
	}
}

func BenchmarkVerify(b *testing.B) {
	v := verifier(ticket.NewKeyRing(iss.Keys()))
	tok := iss.Mint(b, claims())
	for b.Loop() {
		if _, err := v.Verify(tok, t0, want); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMint(b *testing.B) {
	c := claims()
	for b.Loop() {
		if _, err := ticket.Mint(iss.Priv, c); err != nil {
			b.Fatal(err)
		}
	}
}

func TestInvalid(t *testing.T) {
	if !ticket.Invalid(ticket.ErrPermission) || !ticket.Invalid(ticket.ErrMalformed) || ticket.Invalid(errors.New("x")) || ticket.Invalid(nil) {
		t.Fatal("Invalid misclassifies")
	}
}

func TestParsePermRoundTrips(t *testing.T) {
	for _, p := range []ticket.Perm{ticket.Read, ticket.Write, ticket.Read | ticket.Write} {
		if got, ok := ticket.ParsePerm(p.String()); !ok || got != p {
			t.Errorf("%v: %v %v", p, got, ok)
		}
	}
	for _, bad := range []string{"", "rw", "write|read", "perm(4)"} {
		if _, ok := ticket.ParsePerm(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
