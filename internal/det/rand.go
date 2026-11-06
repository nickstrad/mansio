package det

import (
	"crypto/rand"
	"encoding/hex"
	mrand "math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
)

// Rand is the randomness port: a math/rand/v2 Source. Build a *rand.Rand over it with rand.New. Every Rand
// here is safe for concurrent use, so one *rand.Rand over it may be shared (math/rand/v2's Rand keeps no state
// of its own).
type Rand = mrand.Source

// SystemRand draws from the runtime's per-thread generator (math/rand/v2's top-level functions).
func SystemRand() Rand { return systemRand{} }

// RandOr returns r, or SystemRand() when r is nil: the default for an optional Rand field.
func RandOr(r Rand) Rand {
	if r == nil {
		return systemRand{}
	}
	return r
}

// NewRand is a PCG source seeded with seed: the same seed gives the same stream.
func NewRand(seed uint64) Rand { return &lockedRand{src: mrand.NewPCG(seed, 0x9E3779B97F4A7C15)} }

type systemRand struct{}

func (systemRand) Uint64() uint64 { return mrand.Uint64() }

type lockedRand struct {
	mu  sync.Mutex
	src *mrand.PCG
}

func (r *lockedRand) Uint64() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.src.Uint64()
}

// IDGen makes ids that must be unique: request ids, today.
type IDGen interface {
	NewID() string
}

// SystemIDs makes tag + 12 random hex digits + "-" + a base-36 counter from 1 (e.g. "c3f9a01b2c4d5-1"): unique
// across processes by the random part, within one by the counter. Safe for concurrent use.
func SystemIDs(tag string) IDGen {
	var b [6]byte
	rand.Read(b[:]) // never fails (Go 1.24+)
	return newSeqIDs(tag, b)
}

// NewIDs is SystemIDs with the random part drawn from r: a seeded r gives the same ids every run.
func NewIDs(tag string, r Rand) IDGen {
	var b [6]byte
	v := r.Uint64()
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return newSeqIDs(tag, b)
}

func newSeqIDs(tag string, b [6]byte) *seqIDs {
	return &seqIDs{prefix: tag + hex.EncodeToString(b[:]) + "-"}
}

type seqIDs struct {
	prefix string
	n      atomic.Uint64
}

func (s *seqIDs) NewID() string { return s.prefix + strconv.FormatUint(s.n.Add(1), 36) }
