package docexec

import (
	"encoding/hex"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// Call is one decoded request as an actor closure: Check is the fingerprint check (it runs before the actor's
// duplicate check, spec §6.1), Fn runs the operations. Use a Call for one actor request only: Check hands the
// schema it found to Fn.
type Call struct {
	Req     *ir.Request
	Schemas *Schemas
	// IDs draws server ids for inserts without a client id (NewUUIDv7 in production).
	IDs det.IDGen
	// Wrap, if set, wraps the actor's Tx for Fn (tests inject statement failures with it).
	Wrap func(Tx) Tx

	schema *Schema
}

// Request is the actor request for c: a write closure with the request id for a mutation, a read closure for a
// query. The caller sets Deadline and StmtTimeout.
func (c *Call) Request() *actor.Request {
	r := &actor.Request{Fn: c.Fn, Check: c.Check, Write: c.Req.Kind == ir.Mutation}
	if r.Write {
		r.RequestID = c.Req.RequestID
	}
	return r
}

// Check implements actor.Request.Check.
func (c *Call) Check(tx *actor.Tx) error {
	s, err := Check(tx, c.Schemas, c.Req.Schema)
	c.schema = s
	return err
}

// Fn implements actor.Func: Output.Body is the canonical JSON of the result array (stored by the actor as the
// request-id record of a mutation), Output.Value the array itself.
//
//	pre: Check succeeded on the same request
func (c *Call) Fn(tx *actor.Tx) (actor.Output, error) {
	var t Tx = tx
	if c.Wrap != nil {
		t = c.Wrap(t)
	}
	result, err := Run(t, c.schema.Schema, c.Req, c.IDs)
	if err != nil {
		return actor.Output{}, err
	}
	return actor.Output{Body: []byte(ir.Canonical(result)), Value: result}, nil
}

// InitFn is the write closure that initializes a new database with s (Init).
func InitFn(s *Schema) actor.Func {
	return func(tx *actor.Tx) (actor.Output, error) { return actor.Output{}, Init(tx, s) }
}

// InitRequest is InitFn as an actor request (no request id).
func InitRequest(s *Schema) *actor.Request { return &actor.Request{Write: true, Fn: InitFn(s)} }

// uuidV7 draws RFC 9562 version-7 UUIDs: 48 bits of Unix milliseconds, then 74 random bits.
type uuidV7 struct {
	clock det.Clock
	rand  det.Rand
}

// NewUUIDv7 returns an IDGen of canonical lowercase UUIDv7 strings (the form insert.id must have), timed by
// clock and randomized by r (det.SystemRand() in production; a seeded det.NewRand in a simulation).
// Safe for concurrent use when r is.
func NewUUIDv7(clock det.Clock, r det.Rand) det.IDGen {
	return uuidV7{clock: det.Or(clock), rand: det.RandOr(r)}
}

func (u uuidV7) NewID() string {
	var b [16]byte
	ms := uint64(u.clock.Now().UnixMilli()) & (1<<48 - 1)
	for i := range 6 {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	hi, lo := u.rand.Uint64(), u.rand.Uint64()
	for i := range 2 {
		b[6+i] = byte(hi >> (8 * i))
	}
	for i := range 8 {
		b[8+i] = byte(lo >> (8 * i))
	}
	b[6] = 0x70 | b[6]&0x0f // version 7
	b[8] = 0x80 | b[8]&0x3f // RFC 9562 variant
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}
