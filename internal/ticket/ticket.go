// Package ticket holds the access ticket of scale-out option 4 (§3.1): a capability, signed by the control
// plane with Ed25519, for one database on one VM at one epoch. The agent verifies it without any network call.
//
// # Encoding (version 1)
//
// A ticket is base64url (no padding, strict) of
//
//	version (1 byte, = 1) | len(kid) (1 byte) | kid | claims | signature (64 bytes)
//	claims = str(tid) str(dbid) str(vm) uvarint(epoch) uvarint(gen) str(sub) perms (1 byte) uvarint(iat) uvarint(exp)
//	str(s)  = uvarint(len(s)) s
//
// The signature is Ed25519 over Context followed by every byte before the signature, so the version and the
// key id are signed too (no downgrade, no key-id swap). Why not JWT or JSON: there is no "alg" field to confuse
// (version 1 means Ed25519, nothing else); the verifier parses only two length bytes and a key id before the
// signature check and decodes claims only from signed bytes; a ticket is ~150–250 bytes instead of ~400; and it
// needs only the standard library. Decoding is canonical: claims must re-encode to exactly the signed bytes
// (minimal varints, no trailing data), so one claim set has exactly one valid ticket per key.
//
// Everything here is pure: the time is an argument and keys come through the KeySet port.
package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// Version is the only encoding this package reads and writes.
const Version = 1

// Context is prepended to the signed bytes (domain separation: a ticket signature cannot be valid for any other
// message the issuer key signs).
const Context = "ts-db access ticket\x00"

// Limits on claim sizes. They bound the work an unauthenticated client can cause.
const (
	MaxKID    = 32
	MaxTID    = 64
	MaxDBID   = 128
	MaxVM     = 64
	MaxSub    = 128
	MaxTicket = 1024    // encoded length; longer input is rejected before decoding
	MaxUnix   = 1 << 40 // latest exp accepted (year ~36,800): keeps time arithmetic far from overflow
)

// Perm is a permission set on one database.
type Perm uint8

const (
	Read  Perm = 1 << iota // reads
	Write                  // writes (a write ticket does not imply reads; issue Read|Write for both)

	allPerms = Read | Write
)

// Has reports whether p includes every permission in need.
func (p Perm) Has(need Perm) bool { return p&need == need }

func (p Perm) String() string {
	switch p {
	case Read:
		return "read"
	case Write:
		return "write"
	case Read | Write:
		return "read|write"
	}
	return fmt.Sprintf("perm(%d)", uint8(p))
}

// Claims are a ticket's contents. Times are Unix seconds.
type Claims struct {
	KID   string // signing key id (not a claim in the payload sense, but signed)
	TID   string // tenant
	DBID  string // immutable database id
	VM    string // audience: the VM that owns the database
	Epoch uint64 // placement epoch; must match the agent's durable state
	Gen   uint64 // generation of the database at the epoch
	Sub   string // principal
	Perms Perm
	IAT   int64 // issued at
	EXP   int64 // expires at
}

// Errors. Each verification failure has its own sentinel (for logs and tests); the front maps them all to the
// one wire code ticket_invalid.
var (
	ErrMalformed   = errors.New("ticket: malformed")
	ErrVersion     = errors.New("ticket: unknown version")
	ErrUnknownKey  = errors.New("ticket: unknown key id")
	ErrSignature   = errors.New("ticket: bad signature")
	ErrClaims      = errors.New("ticket: invalid claims")
	ErrExpired     = errors.New("ticket: expired")
	ErrNotYetValid = errors.New("ticket: issued in the future")
	ErrTTL         = errors.New("ticket: lifetime longer than allowed")
	ErrAudience    = errors.New("ticket: issued for another VM")
	ErrDatabase    = errors.New("ticket: issued for another database")
	ErrPermission  = errors.New("ticket: permission not granted")
)

var b64 = base64.RawURLEncoding.Strict()

// Validate reports claims that no issuer should sign: empty or oversized strings, no or unknown permissions,
// negative times, exp not after iat.
func (c Claims) Validate() error {
	switch {
	case c.KID == "" || len(c.KID) > MaxKID:
		return fmt.Errorf("kid must be 1..%d bytes", MaxKID)
	case c.TID == "" || len(c.TID) > MaxTID:
		return fmt.Errorf("tid must be 1..%d bytes", MaxTID)
	case c.DBID == "" || len(c.DBID) > MaxDBID:
		return fmt.Errorf("dbid must be 1..%d bytes", MaxDBID)
	case c.VM == "" || len(c.VM) > MaxVM:
		return fmt.Errorf("vm must be 1..%d bytes", MaxVM)
	case c.Sub == "" || len(c.Sub) > MaxSub:
		return fmt.Errorf("sub must be 1..%d bytes", MaxSub)
	case c.Perms == 0 || c.Perms&^allPerms != 0:
		return fmt.Errorf("perms %d: want a non-empty subset of read|write", c.Perms)
	case c.IAT < 0 || c.EXP <= c.IAT || c.EXP > MaxUnix:
		return fmt.Errorf("need 0 <= iat < exp <= %d, got iat %d exp %d", int64(MaxUnix), c.IAT, c.EXP)
	}
	return nil
}

// Mint signs c with priv. For the control plane, tests and tools; the agent never mints.
//
//	post: err == nil ⇒ Parse(token) == c, and Verify accepts token for c's db, vm and perms within [iat, exp)
func Mint(priv ed25519.PrivateKey, c Claims) (string, error) {
	if err := c.Validate(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrClaims, err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("ticket: private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	body := encode(c)
	sig := ed25519.Sign(priv, signed(body))
	return b64.EncodeToString(append(body, sig...)), nil
}

// encode writes everything before the signature.
func encode(c Claims) []byte {
	b := make([]byte, 0, 2+len(c.KID)+len(c.TID)+len(c.DBID)+len(c.VM)+len(c.Sub)+5*binary.MaxVarintLen64+1+ed25519.SignatureSize)
	b = append(b, Version, byte(len(c.KID)))
	b = append(b, c.KID...)
	str := func(s string) { b = binary.AppendUvarint(b, uint64(len(s))); b = append(b, s...) }
	str(c.TID)
	str(c.DBID)
	str(c.VM)
	b = binary.AppendUvarint(b, c.Epoch)
	b = binary.AppendUvarint(b, c.Gen)
	str(c.Sub)
	b = append(b, byte(c.Perms))
	b = binary.AppendUvarint(b, uint64(c.IAT))
	b = binary.AppendUvarint(b, uint64(c.EXP))
	return b
}

func signed(body []byte) []byte { return append([]byte(Context), body...) }

// split decodes token into the signed body, the key id and the signature, checking only framing.
func split(token string) (body []byte, kid string, sig []byte, err error) {
	if len(token) > MaxTicket {
		return nil, "", nil, ErrMalformed
	}
	for i := 0; i < len(token); i++ { // the decoder skips \r and \n: without this one ticket has many spellings
		if c := token[i]; !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return nil, "", nil, ErrMalformed
		}
	}
	raw, err := b64.DecodeString(token)
	if err != nil || len(raw) < 2+ed25519.SignatureSize {
		return nil, "", nil, ErrMalformed
	}
	if raw[0] != Version {
		return nil, "", nil, ErrVersion
	}
	n := int(raw[1])
	if n == 0 || n > MaxKID || 2+n > len(raw)-ed25519.SignatureSize {
		return nil, "", nil, ErrMalformed
	}
	cut := len(raw) - ed25519.SignatureSize
	return raw[:cut], string(raw[2 : 2+n]), raw[cut:], nil
}

// decode parses a body that encode produced. It is only called on signed bytes, but is still total: any
// input returns claims or ErrMalformed.
func decode(body []byte) (Claims, error) {
	var c Claims
	n := int(body[1])
	c.KID = string(body[2 : 2+n])
	p := body[2+n:]
	bad := false
	uv := func() uint64 {
		v, k := binary.Uvarint(p)
		if k <= 0 {
			bad = true
			return 0
		}
		p = p[k:]
		return v
	}
	str := func(limit int) string {
		l := uv()
		if bad || l > uint64(limit) || l > uint64(len(p)) {
			bad = true
			return ""
		}
		s := string(p[:l])
		p = p[l:]
		return s
	}
	c.TID = str(MaxTID)
	c.DBID = str(MaxDBID)
	c.VM = str(MaxVM)
	c.Epoch = uv()
	c.Gen = uv()
	c.Sub = str(MaxSub)
	if bad || len(p) == 0 {
		return Claims{}, ErrMalformed
	}
	c.Perms = Perm(p[0])
	p = p[1:]
	iat, exp := uv(), uv()
	if bad || len(p) != 0 || iat > math.MaxInt64 || exp > math.MaxInt64 {
		return Claims{}, ErrMalformed
	}
	c.IAT, c.EXP = int64(iat), int64(exp)
	if string(encode(c)) != string(body) { // canonical: minimal varints, nothing extra
		return Claims{}, ErrMalformed
	}
	return c, nil
}

// Parse decodes a ticket without checking its signature or validity. For tools and tests only.
func Parse(token string) (Claims, error) {
	body, _, _, err := split(token)
	if err != nil {
		return Claims{}, err
	}
	return decode(body)
}

// KeySet is the verifier's port to the issuer's public keys. During a rotation it holds the old and the new
// key id at once, so tickets signed by either verify.
type KeySet interface {
	Key(kid string) (ed25519.PublicKey, bool)
}

// Want is what the request needs the ticket to grant.
type Want struct {
	DB   string
	Perm Perm
}

// Verifier checks tickets for one VM.
type Verifier struct {
	Keys   KeySet
	VM     string        // audience: this VM
	Skew   time.Duration // tolerated clock difference with the issuer, both ways; >= 0
	MaxTTL time.Duration // longest exp - iat accepted; 0 = no limit
}

// Verify checks token at time now and returns its claims.
//
//	post: err == nil ⇒ the signature is valid under Keys.Key(c.KID), c.Validate() == nil, c.VM == v.VM,
//	      c.DBID == want.DB, c.Perms.Has(want.Perm), iat - Skew <= now < exp + Skew, exp - iat <= MaxTTL
//	post: err != nil ⇒ errors.Is(err, exactly one of the sentinels above)
func (v Verifier) Verify(token string, now time.Time, want Want) (Claims, error) {
	body, kid, sig, err := split(token)
	if err != nil {
		return Claims{}, err
	}
	pub, ok := v.Keys.Key(kid)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return Claims{}, ErrUnknownKey
	}
	if !ed25519.Verify(pub, signed(body), sig) {
		return Claims{}, ErrSignature
	}
	c, err := decode(body)
	if err != nil {
		return Claims{}, err
	}
	if err := v.check(c, now, want); err != nil {
		return Claims{}, err
	}
	return c, nil
}

// check applies every rule except the signature to decoded claims.
func (v Verifier) check(c Claims, now time.Time, want Want) error {
	if c.Validate() != nil {
		return ErrClaims
	}
	if err := v.checkTime(c, now); err != nil {
		return err
	}
	switch {
	case c.VM != v.VM:
		return ErrAudience
	case c.DBID != want.DB:
		return ErrDatabase
	case want.Perm == 0 || !c.Perms.Has(want.Perm):
		return ErrPermission
	}
	return nil
}

// checkTime applies expiry, not-before and the lifetime cap.
//
//	expired       ⇔ now >= exp + Skew
//	not yet valid ⇔ now <  iat - Skew
func (v Verifier) checkTime(c Claims, now time.Time) error {
	skew := max(v.Skew, 0)
	if !now.Before(time.Unix(c.EXP, 0).Add(skew)) {
		return ErrExpired
	}
	if now.Before(time.Unix(c.IAT, 0).Add(-skew)) {
		return ErrNotYetValid
	}
	if v.MaxTTL > 0 && c.EXP-c.IAT > int64(v.MaxTTL/time.Second) {
		return ErrTTL
	}
	return nil
}

var invalid = []error{ErrMalformed, ErrVersion, ErrUnknownKey, ErrSignature, ErrClaims, ErrExpired, ErrNotYetValid,
	ErrTTL, ErrAudience, ErrDatabase, ErrPermission}

// Invalid reports whether err is one of this package's verification failures.
func Invalid(err error) bool {
	for _, s := range invalid {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
