// Package durabletest has a fake durable.Store with fault switches and the contract suite every Store
// implementation must pass. The fake is mutex-guarded so -race stays clean, and its conditional publish is
// exact: version check, epoch check and write happen under one lock.
package durabletest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
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
	Delay           time.Duration // every call sleeps this long first (or until ctx ends)
}

// Store keeps objects and manifests in memory.
type Store struct {
	mu        sync.Mutex
	objects   map[string][]byte          // object name → bytes
	meta      map[string]durable.GenInfo // object name → metadata recorded at upload
	manifests map[string]stored
	next      uint64 // version counter
	f         Faults

	puts, publishes, fetches int
}

type stored struct {
	m durable.Manifest
	v durable.Version
}

func NewStore() *Store {
	return &Store{objects: map[string][]byte{}, meta: map[string]durable.GenInfo{}, manifests: map[string]stored{}}
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
	st.v = durable.Version(strconv.FormatUint(s.next, 10))
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
	m := durable.NextManifest(key, gen, epoch, DigestOf(data), time.Now())
	s.objects[m.Object] = bytes.Clone(data)
	s.meta[m.Object] = m.ObjectInfo()
	s.next++
	s.manifests[key] = stored{m, durable.Version(strconv.FormatUint(s.next, 10))}
	return m
}

// DigestOf is data's size and sha256.
func DigestOf(data []byte) durable.Digest {
	h := sha256.Sum256(data)
	return durable.Digest{Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}
}

// begin applies Delay and Down; it returns a snapshot of the switches.
func (s *Store) begin(ctx context.Context) (Faults, error) {
	s.mu.Lock()
	f := s.f
	s.mu.Unlock()
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return f, ctx.Err()
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
	f, err := s.begin(ctx)
	if err != nil {
		return durable.Manifest{}, durable.NoVersion, err
	}
	if f.FailRead {
		return durable.Manifest{}, durable.NoVersion, fmt.Errorf("read manifest %s: %w", key, ErrInjected)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.manifests[key]
	if !ok {
		return durable.Manifest{}, durable.NoVersion, fmt.Errorf("read manifest %s: %w", key, durable.ErrNoManifest)
	}
	return st.m, st.v, nil
}

func (s *Store) PutGeneration(ctx context.Context, m durable.Manifest, path string) error {
	f, err := s.begin(ctx)
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
	s.puts++
	if f.LoseUploadReply {
		return fmt.Errorf("upload %s: reply lost: %w", m.Object, ErrInjected)
	}
	return nil
}

func (s *Store) StatGeneration(ctx context.Context, object string) (durable.GenInfo, error) {
	if _, err := s.begin(ctx); err != nil {
		return durable.GenInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[object]
	if !ok {
		return durable.GenInfo{}, fmt.Errorf("stat %s: %w", object, durable.ErrNotFound)
	}
	info := s.meta[object]
	info.Size = int64(len(data)) // the real size, even after Corrupt
	return info, nil
}

func (s *Store) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (durable.Fetched, error) {
	t0 := time.Now()
	f, err := s.begin(ctx)
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
	ttfb := time.Since(t0)
	tmp := path + ".part"
	if f.FailFetch {
		os.WriteFile(tmp, data[:len(data)/2], 0o644)
		os.Remove(tmp)
		return durable.Fetched{}, fmt.Errorf("fetch %s: %w", m.Object, ErrInjected)
	}
	if got := DigestOf(data); got != (durable.Digest{Size: m.Size, SHA256: m.SHA256}) {
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
	return durable.Fetched{Bytes: int64(len(data)), TTFB: ttfb, Total: time.Since(t0)}, nil
}

func (s *Store) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	f, err := s.begin(ctx)
	if err != nil {
		return durable.NoVersion, err
	}
	if f.FailPublish {
		return durable.NoVersion, fmt.Errorf("publish %s: %w", m.Key, ErrInjected)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.manifests[m.Key]
	switch {
	case st.v != prev:
		return durable.NoVersion, fmt.Errorf("publish %s: stored version %q, caller read %q: %w", m.Key, st.v, prev, durable.ErrStale)
	case ok && st.m.Epoch > m.Epoch:
		return durable.NoVersion, fmt.Errorf("publish %s: stored epoch %d > %d: %w", m.Key, st.m.Epoch, m.Epoch, durable.ErrStale)
	}
	s.next++
	v := durable.Version(strconv.FormatUint(s.next, 10))
	s.manifests[m.Key] = stored{m, v}
	s.publishes++
	switch {
	case f.LosePublishReply:
		return durable.NoVersion, fmt.Errorf("publish %s: reply lost: %w", m.Key, ErrInjected)
	case f.ApplyThenStale:
		return durable.NoVersion, fmt.Errorf("publish %s: reply lost, the re-send found version %q: %w", m.Key, v, durable.ErrStale)
	}
	return v, nil
}
