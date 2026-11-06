package docexec

import (
	"encoding/hex"

	"github.com/nickstrad/mansio/server/internal/det"
)

// uuidV7 draws RFC 9562 version-7 UUIDs: 48 bits of Unix milliseconds, then 74 random bits.
type uuidV7 struct {
	clock det.Clock
	rand  det.Rand
}

// NewUUIDv7 returns an IDGen of canonical lowercase UUIDv7 strings (the form insert.id must have), timed by
// clock and randomized by r (det.SystemRand() in production; a seeded det.NewRand in a simulation).
// Safe for concurrent use when r is.
//
// There is no in-millisecond counter (RFC 9562 method 1 is not used): ids drawn within one millisecond are
// unordered among themselves, and uniqueness rests on the 74 random bits. A collision (≈ 2^-37 at 2^18 ids in
// one ms in one table) fails that insert's statement on the primary key: a StorageError, nothing committed.
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
