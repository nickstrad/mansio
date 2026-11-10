// Package durabletest has a fake durable.Store with fault switches and the contract suite every Store
// implementation must pass. The fake is mutex-guarded so -race stays clean, and its conditional publish is
// exact: version check, epoch check and write happen under one lock.
package durabletest

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/durable"
)

// ErrInjected is returned by every switch-triggered failure.
var ErrInjected = errors.New("durabletest: injected failure")

// Faults are the fake's switches. The zero value is a healthy store.
type Faults struct {
	Down             bool // every call fails with ErrInjected
	FailRead         bool // ReadManifest fails
	FailUpload       bool // PutGeneration fails part-way: nothing becomes visible (as with one S3 PUT)
	FailFetch        bool // FetchGeneration fails after writing part of the file (it must clean up)
	FailPublish      bool // PublishManifest fails without applying (a crash between upload and publish)
	LosePublishReply bool // PublishManifest applies, then reports ErrInjected (ambiguous outcome)
	// ApplyThenStale: PublishManifest applies, then reports ErrStale. That is what a client-side retry of a
	// publish whose reply was lost sees: the first attempt moved the version, so the re-send gets 412.
	ApplyThenStale bool
	// LoseUploadReply: PutGeneration stores the object, then reports ErrInjected (ambiguous outcome).
	LoseUploadReply bool
	FailList        bool          // ListKey and ListKeys fail
	FailDelete      bool          // DeleteGenerations deletes the first half of the names, then fails
	Delay           time.Duration // every call sleeps this long first (or until ctx ends), on the store's clock
	// LandLate: PublishManifest answers ErrInjected at once (no reply) but the request is still in flight: it
	// lands LandLate later on the store's clock, its condition checked then, like a PUT S3 completes after the
	// client gave up. A zombie of a crashed process lands too.
	LandLate time.Duration
	// LandTag labels a LandLate request for the predicate SetLandIf installs (a simulation tags each request
	// with the process that sent it, and decides whether a dead process's request still lands).
	LandTag uint64
}

// CallFaults decides the switches for one call (SetCallFaults): op is the call's durable.Op* name
// (PublishManifest is durable.OpPublish), key the database it is for, static the switches SetFaults set.
type CallFaults func(op, key string, static Faults) Faults

var _ durable.GCStore = (*Store)(nil)

// Store keeps objects and manifests in memory.
type Store struct {
	mu        sync.Mutex
	objects   map[string][]byte          // object name → bytes
	meta      map[string]durable.GenInfo // object name → metadata recorded at upload
	manifests map[string]stored
	mod       map[string]time.Time // object name → when it was stored (the store's LastModified)
	next      uint64               // version counter
	f         Faults
	call      CallFaults // nil: every call gets f
	landIf    func(tag uint64) bool
	denyHead  bool             // SetDenyMissingHead
	clk       det.Clock        // Delay waits and fetch timings
	clock     func() time.Time // stamps LastModified (and Seed's manifest)

	puts, publishes, fetches int
	requests                 map[string]int64 // calls by durable.Op* name, failed ones included (RequestCounts)
}

type stored struct {
	m   durable.Manifest
	v   durable.Version
	mod time.Time
	bad bool // the stored bytes do not decode (CorruptManifest)
}

func NewStore() *Store {
	return &Store{objects: map[string][]byte{}, meta: map[string]durable.GenInfo{}, manifests: map[string]stored{},
		mod: map[string]time.Time{}, clk: det.System(), clock: time.Now, requests: map[string]int64{}}
}

// UseClock puts the whole store on c: Delay waits (so a stall ends when c reaches it, or its ctx ends on c),
// fetch timings, and the LastModified stamps (as SetClock(c.Now)). A simulation passes its manual clock.
func (s *Store) UseClock(c det.Clock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clk, s.clock = det.Or(c), det.Or(c).Now
}

// ErrAccessDenied is what StatGeneration answers for a missing object under SetDenyMissingHead.
var ErrAccessDenied = errors.New("durabletest: 403 AccessDenied")

// SetDenyMissingHead makes StatGeneration of a missing object answer ErrAccessDenied instead of
// durable.ErrNotFound, as S3 answers a HEAD (403, not 404) when the identity lacks s3:ListBucket.
func (s *Store) SetDenyMissingHead(deny bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denyHead = deny
}

// SetLandIf installs the predicate asked when a LandLate request lands, with its LandTag; false drops it. nil:
// every one lands.
func (s *Store) SetLandIf(f func(tag uint64) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.landIf = f
}

// SetCallFaults has f decide the switches of every later call (nil: back to SetFaults alone). f runs once per
// call, before the call does anything, outside the store's lock; a deterministic simulation draws per-call
// faults (a lost reply, one stalled request) from its own seeded source there.
func (s *Store) SetCallFaults(f CallFaults) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.call = f
}

// SetClock sets the store's own clock, which stamps LastModified on every write (the server's clock in S3).
// Delay still waits on the clock UseClock set (default: real time).
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = now
}

// Names returns the names of the stored generation objects of key, sorted (test inspection).
func (s *Store) Names(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.objects {
		if strings.HasPrefix(n, durable.GenPrefix(key)) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// SetModified backdates (or postdates) a stored object's LastModified (tests of the grace window).
func (s *Store) SetModified(name string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mod[name] = t
}

// SetFaults replaces the switches.
func (s *Store) SetFaults(f Faults) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f = f
}

// Manifest returns key's stored manifest, if any (test inspection; no faults apply).
func (s *Store) Manifest(key string) (durable.Manifest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.manifests[key]
	return st.m, ok
}

// Object returns a stored object's bytes, if any.
func (s *Store) Object(name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[name]
	return bytes.Clone(b), ok
}

// Counts returns how many uploads, publishes and fetches succeeded.
func (s *Store) Counts() (puts, publishes, fetches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts, s.publishes, s.fetches
}

// DeleteObject removes an object (simulates a lost generation).
func (s *Store) DeleteObject(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, name)
	delete(s.meta, name)
	delete(s.mod, name)
}

// CorruptManifest makes key's stored manifest undecodable (a new version): ReadManifest then fails with
// ErrBadManifest, and only a publish conditional on that version replaces it.
func (s *Store) CorruptManifest(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.manifests[key]
	s.next++
	st.v, st.mod, st.bad = durable.Version(strconv.FormatUint(s.next, 10)), s.clock(), true
	s.manifests[key] = st
}

// DeleteManifest removes key's manifest (simulates a lost manifest).
func (s *Store) DeleteManifest(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.manifests, key)
}

// Corrupt flips one byte of an object (or appends one to an empty object).
func (s *Store) Corrupt(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.objects[name]
	if len(b) == 0 {
		s.objects[name] = []byte{1}
		return
	}
	b[0] ^= 0xff
}

// Bump raises key's stored epoch as another owner would (an external Stamp). No manifest: creates a
// generation-0 one.
func (s *Store) Bump(key string, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.manifests[key]
	if st.v == durable.NoVersion {
		st.m = durable.Manifest{Key: key}
	}
	st.m.Epoch = epoch
	s.next++
	st.v, st.mod = durable.Version(strconv.FormatUint(s.next, 10)), s.clock()
	s.manifests[key] = st
}

// Seed publishes data as key's next generation at epoch, bypassing faults. Returns the manifest.
func (s *Store) Seed(key string, epoch uint64, data []byte) durable.Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.manifests[key]
	gen := st.m.Generation + 1
	for {
		if _, taken := s.objects[durable.GenObject(key, gen, epoch)]; !taken {
			break
		}
		gen++
	}
	m := durable.NextManifest(key, gen, epoch, DigestOf(data), s.clock()).Succeeding(&st.m)
	s.objects[m.Object] = bytes.Clone(data)
	s.meta[m.Object] = m.ObjectInfo()
	s.mod[m.Object] = s.clock()
	s.next++
	s.manifests[key] = stored{m: m, v: durable.Version(strconv.FormatUint(s.next, 10)), mod: s.clock()}
	return m
}

// DigestOf is data's size, sha256 and md5.
func DigestOf(data []byte) durable.Digest {
	h, m := sha256.Sum256(data), md5.Sum(data)
	return durable.Digest{Size: int64(len(data)), SHA256: hex.EncodeToString(h[:]), MD5: hex.EncodeToString(m[:])}
}

// RequestCounts is requests by operation (durable.RequestCounter), shaped like objstore.Durable's with no
// retries: one per call, except that PutGeneration also verifies by HEAD and PublishManifest first re-reads
// the manifest, so a write-back is four requests here as on S3.
func (s *Store) RequestCounts() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.requests)
}

// begin counts one request of each op, decides the call's switches (SetFaults, then SetCallFaults with the
// last op), applies Delay and Down, and returns the switches.
func (s *Store) begin(ctx context.Context, key string, ops ...string) (Faults, error) {
	s.mu.Lock()
	for _, op := range ops {
		s.requests[op]++
	}
	f, call, clk := s.f, s.call, s.clk
	s.mu.Unlock()
	if call != nil {
		f = call(ops[len(ops)-1], key, f)
	}
	if f.Delay > 0 {
		if err := det.Sleep(ctx, clk, f.Delay); err != nil {
			return f, err
		}
	}
	if err := ctx.Err(); err != nil {
		return f, err
	}
	if f.Down {
		return f, fmt.Errorf("store down: %w", ErrInjected)
	}
	return f, nil
}

func (s *Store) ReadManifest(ctx context.Context, key string) (durable.Manifest, durable.Version, error) {
	f, err := s.begin(ctx, key, durable.OpGetManifest)
	if err != nil {
		return durable.Manifest{}, durable.NoVersion, err
	}
	if f.FailRead {
		return durable.Manifest{}, durable.NoVersion, fmt.Errorf("read manifest %s: %w", key, ErrInjected)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.manifests[key]
	switch {
	case !ok:
		return durable.Manifest{}, durable.NoVersion, fmt.Errorf("read manifest %s: %w", key, durable.ErrNoManifest)
	case st.bad:
		return durable.Manifest{}, durable.NoVersion, fmt.Errorf("read manifest %s: %w", key, durable.ErrBadManifest)
	}
	return st.m, st.v, nil
}

func (s *Store) PutGeneration(ctx context.Context, m durable.Manifest, path string) error {
	f, err := s.begin(ctx, m.Key, durable.OpPutGen)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if f.FailUpload {
		return fmt.Errorf("upload %s: connection reset after %d of %d bytes: %w", m.Object, len(data)/2, len(data), ErrInjected)
	}
	if got := DigestOf(data); got != m.Digest() {
		return fmt.Errorf("upload %s: store computed %+v, caller sent %+v", m.Object, got, m.Digest())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[m.Object]; ok {
		return fmt.Errorf("upload %s: %w", m.Object, durable.ErrExists)
	}
	s.objects[m.Object] = data
	s.meta[m.Object] = m.ObjectInfo()
	s.mod[m.Object] = s.clock()
	s.puts++
	s.requests[durable.OpHead]++ // the adapter's verifying HEAD
	if f.LoseUploadReply {
		return fmt.Errorf("upload %s: reply lost: %w", m.Object, ErrInjected)
	}
	return nil
}

// keyOf is the key a stored generation object belongs to ("" if it is not stored).
func (s *Store) keyOf(object string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta[object].Key
}

func (s *Store) StatGeneration(ctx context.Context, object string) (durable.GenInfo, error) {
	if _, err := s.begin(ctx, s.keyOf(object), durable.OpHead); err != nil {
		return durable.GenInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[object]
	if !ok && s.denyHead {
		return durable.GenInfo{}, fmt.Errorf("stat %s: %w", object, ErrAccessDenied)
	}
	if !ok {
		return durable.GenInfo{}, fmt.Errorf("stat %s: %w", object, durable.ErrNotFound)
	}
	info := s.meta[object]
	info.Size = int64(len(data)) // the real size, even after Corrupt
	return info, nil
}

func (s *Store) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (durable.Fetched, error) {
	s.mu.Lock()
	clk := s.clk
	s.mu.Unlock()
	t0 := clk.Now()
	f, err := s.begin(ctx, m.Key, durable.OpFetch)
	if err != nil {
		return durable.Fetched{}, err
	}
	s.mu.Lock()
	data, ok := s.objects[m.Object]
	data = bytes.Clone(data)
	s.mu.Unlock()
	if !ok {
		return durable.Fetched{}, fmt.Errorf("fetch %s: %w", m.Object, durable.ErrNotFound)
	}
	ttfb := det.Since(clk, t0)
	tmp := path + ".part"
	if f.FailFetch {
		os.WriteFile(tmp, data[:len(data)/2], 0o644)
		os.Remove(tmp)
		return durable.Fetched{}, fmt.Errorf("fetch %s: %w", m.Object, ErrInjected)
	}
	if got := DigestOf(data); got.Size != m.Size || got.SHA256 != m.SHA256 {
		return durable.Fetched{}, fmt.Errorf("fetch %s: got %d bytes sha256 %s: %w", m.Object, got.Size, got.SHA256, durable.ErrCorrupt)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return durable.Fetched{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return durable.Fetched{}, err
	}
	s.mu.Lock()
	s.fetches++
	s.mu.Unlock()
	return durable.Fetched{Bytes: int64(len(data)), TTFB: ttfb, Total: det.Since(clk, t0)}, nil
}

func (s *Store) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	f, err := s.begin(ctx, m.Key, durable.OpGetManifest, durable.OpPublish)
	if err != nil {
		return durable.NoVersion, err
	}
	if f.FailPublish {
		return durable.NoVersion, fmt.Errorf("publish %s: %w", m.Key, ErrInjected)
	}
	if f.LandLate > 0 {
		s.mu.Lock()
		clk := s.clk
		s.mu.Unlock()
		tag := f.LandTag
		clk.AfterFunc(f.LandLate, func() {
			s.mu.Lock()
			land := s.landIf
			s.mu.Unlock()
			if land == nil || land(tag) {
				_, _ = s.publish(m, prev, Faults{})
			}
		})
		return durable.NoVersion, fmt.Errorf("publish %s: no reply (the request is still in flight): %w", m.Key, ErrInjected)
	}
	return s.publish(m, prev, f)
}

// publish applies a conditional publish under the lock, then reports as f says.
func (s *Store) publish(m durable.Manifest, prev durable.Version, f Faults) (durable.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.manifests[m.Key]
	switch {
	case st.v != prev:
		return durable.NoVersion, fmt.Errorf("publish %s: stored version %q, caller read %q: %w", m.Key, st.v, prev, durable.ErrStale)
	case ok && !st.bad && st.m.Epoch > m.Epoch:
		return durable.NoVersion, fmt.Errorf("publish %s: stored epoch %d > %d: %w", m.Key, st.m.Epoch, m.Epoch, durable.ErrStale)
	}
	s.next++
	v := durable.Version(strconv.FormatUint(s.next, 10))
	s.manifests[m.Key] = stored{m: m, v: v, mod: s.clock()}
	s.publishes++
	switch {
	case f.LosePublishReply:
		return durable.NoVersion, fmt.Errorf("publish %s: reply lost: %w", m.Key, ErrInjected)
	case f.ApplyThenStale:
		return durable.NoVersion, fmt.Errorf("publish %s: reply lost, the re-send found version %q: %w", m.Key, v, durable.ErrStale)
	}
	return v, nil
}

func (s *Store) DeleteGenerations(ctx context.Context, key string, names []string) error {
	f, err := s.begin(ctx, key, durable.OpDelete)
	if err != nil {
		return err
	}
	if err := durable.CheckDeletable(key, names); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range names {
		if f.FailDelete && i >= len(names)/2 {
			return fmt.Errorf("delete %s: %w", n, ErrInjected)
		}
		delete(s.objects, n)
		delete(s.meta, n)
		delete(s.mod, n)
	}
	return nil
}

func (s *Store) ListKey(ctx context.Context, key string) (durable.Listing, error) {
	f, err := s.begin(ctx, key, durable.OpList)
	if err != nil {
		return durable.Listing{}, err
	}
	if f.FailList {
		return durable.Listing{}, fmt.Errorf("list %s: %w", key, ErrInjected)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var l durable.Listing
	if st, ok := s.manifests[key]; ok {
		l.Manifest = &durable.StoredObject{Name: durable.ManifestObject(key), Size: int64(len(durable.Encode(st.m))), Modified: st.mod, Version: st.v}
	}
	for n, b := range s.objects {
		if strings.HasPrefix(n, durable.GenPrefix(key)) {
			l.Generations = append(l.Generations, durable.StoredObject{Name: n, Size: int64(len(b)), Modified: s.mod[n]})
		}
	}
	slices.SortFunc(l.Generations, func(a, b durable.StoredObject) int { return strings.Compare(a.Name, b.Name) })
	return l, nil
}

func (s *Store) ListKeys(ctx context.Context, shard string, fn func(key string) error) error {
	f, err := s.begin(ctx, "", durable.OpList)
	if err != nil {
		return err
	}
	if f.FailList {
		return fmt.Errorf("list shard %s: %w", shard, ErrInjected)
	}
	s.mu.Lock()
	seen := map[string]bool{}
	for k := range s.manifests {
		seen[k] = true
	}
	for _, g := range s.meta {
		seen[g.Key] = true
	}
	var keys []string
	for k := range seen {
		if strings.HasPrefix(durable.KeyPrefix(k), "dbs/"+shard+"/") {
			keys = append(keys, k)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(durable.KeyPrefix(a), durable.KeyPrefix(b)) })
	for _, k := range keys {
		if err := fn(k); err != nil {
			return err
		}
	}
	return nil
}
