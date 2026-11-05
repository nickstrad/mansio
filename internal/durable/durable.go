// Package durable is the durable copy of one database in object storage: immutable generations plus a
// manifest that names the current one (contract 1 in shared_state.md, 2026-11-05).
//
// Layout per database key:
//
//	dbs/<key>/gen/<generation, 20 digits>.db   immutable SQLite snapshot, created once, never overwritten
//	dbs/<key>/manifest.json                     {key, generation, epoch, object, size, sha256, written_at}
//
// Activation reads the manifest and fetches its generation. Write-back uploads a new generation, then
// publishes a manifest naming it, conditionally: the publish fails with ErrStale if the stored manifest
// changed since it was read, or if the stored epoch is greater than the writer's. This package does not
// decide ownership (internal/owner does); it only never lets a lower epoch overwrite a higher one.
//
// Domain code: the decisions here (CheckPublish, CheckStamp, NextManifest) are pure. Storage is reached
// through the Store port; durabletest has a fake and the contract suite, internal/objstore the S3 adapter.
package durable

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickstrad/ts-db-go/internal/errs"
)

const module = "durable"

var (
	// ErrStale: the stored manifest changed since the caller read it, or a higher epoch owns it. Nothing was
	// published. Retrying blindly is not safe: re-read the manifest (and ownership) first.
	ErrStale = errors.New("durable: manifest changed or a higher epoch owns it")
	// ErrNoManifest: the key has no manifest (the database was never written back).
	ErrNoManifest = errors.New("durable: no manifest")
	// ErrNotFound: a generation object the manifest names does not exist.
	ErrNotFound = errors.New("durable: generation object not found")
	// ErrExists: PutGeneration found the generation object already there (an orphan or another writer's).
	// Nothing was overwritten.
	ErrExists = errors.New("durable: generation object already exists")
	// ErrCorrupt: a fetched generation's size or sha256 does not match its manifest.
	ErrCorrupt = errors.New("durable: generation does not match its manifest")
)

// Version identifies one stored manifest for a conditional publish (an S3 ETag, a counter in the fake).
// NoVersion means "no manifest exists".
type Version string

const NoVersion Version = ""

// Manifest names a key's current generation. Generation 0 is an empty database with no object.
type Manifest struct {
	Key        string    `json:"key"`
	Generation uint64    `json:"generation"`
	Epoch      uint64    `json:"epoch"`
	Object     string    `json:"object"` // GenObject(Key, Generation); "" for generation 0
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"` // lower-case hex; "" for generation 0
	WrittenAt  time.Time `json:"written_at"`
}

// Digest is what a snapshot file holds.
type Digest struct {
	Size   int64
	SHA256 string // lower-case hex
}

// Fetched describes one generation download.
type Fetched struct {
	Bytes int64
	TTFB  time.Duration
	Total time.Duration
}

// Store is the port to object storage. Implementations: durabletest.Store (fake), objstore.Durable (S3).
// Every implementation must pass durabletest.RunStoreSuite.
type Store interface {
	// ReadManifest returns key's manifest and its version.
	//
	//	post: no manifest ⇒ errors.Is(err, ErrNoManifest)
	//	post: err == nil ⇒ v != NoVersion and m.Key == key
	ReadManifest(ctx context.Context, key string) (m Manifest, v Version, err error)

	// PutGeneration uploads the file at path as GenObject(key, gen), only if that object does not exist.
	//
	//	pre:  d is the file's size and sha256; gen >= 1
	//	post: the object already existed ⇒ errors.Is(err, ErrExists), nothing overwritten
	//	post: err == nil ⇒ the object holds the file's bytes (size verified; sha256 sent for the store to check)
	//	post: other err ⇒ the object may or may not exist; it is not referenced by any manifest
	PutGeneration(ctx context.Context, key string, gen uint64, path string, d Digest) error

	// FetchGeneration downloads m.Object to path and checks it against m.Size and m.SHA256.
	//
	//	pre:  m.Generation >= 1
	//	post: err == nil ⇒ path holds exactly the generation's bytes
	//	post: err != nil ⇒ path does not exist (no partial file, no .part)
	//	post: missing object ⇒ ErrNotFound; size or hash mismatch ⇒ ErrCorrupt
	FetchGeneration(ctx context.Context, m Manifest, path string) (Fetched, error)

	// PublishManifest stores m as m.Key's manifest if the stored version is still prev (NoVersion: only if
	// none exists) and the stored epoch is not greater than m.Epoch.
	//
	//	post: version moved or stored epoch > m.Epoch ⇒ errors.Is(err, ErrStale), nothing changed
	//	post: err == nil ⇒ the stored manifest is m and v is its new version (v != prev, v != NoVersion)
	//	post: other err ⇒ ambiguous: the publish may or may not have happened; re-read to find out
	PublishManifest(ctx context.Context, m Manifest, prev Version) (v Version, err error)
}

// ValidateKey checks a database key at the edge. Keys become object names, so they must be non-empty
// path segments without "..", empty segments, or a leading or trailing "/".
func ValidateKey(key string) error {
	if key == "" || len(key) > 512 {
		return errs.Wrap(module, nil, "invalid database key %q: must be 1-512 bytes", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errs.Wrap(module, nil, "invalid database key %q: empty, \".\" or \"..\" path segment", key)
		}
	}
	if strings.ContainsAny(key, "\\\x00") {
		return errs.Wrap(module, nil, "invalid database key %q: backslash or NUL", key)
	}
	return nil
}

// GenObject is the object name of key's generation gen.
func GenObject(key string, gen uint64) string { return fmt.Sprintf("dbs/%s/gen/%020d.db", key, gen) }

// ManifestObject is the object name of key's manifest.
func ManifestObject(key string) string { return "dbs/" + key + "/manifest.json" }

// Prefix is the object prefix of everything stored for key.
func Prefix(key string) string { return "dbs/" + key + "/" }

// Encode is the manifest's JSON.
func Encode(m Manifest) []byte {
	b, _ := json.Marshal(m) // cannot fail: plain fields only
	return b
}

// Decode parses and validates a stored manifest (it came from storage: input, not a contract).
func Decode(key string, b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, errs.Wrap(module, err, "manifest of %s is not valid JSON", key)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	if m.Key != key {
		return Manifest{}, errs.Wrap(module, nil, "manifest stored for %s names key %s", key, m.Key)
	}
	return m, nil
}

// Validate checks a manifest's fields agree with each other.
func (m Manifest) Validate() error {
	if err := ValidateKey(m.Key); err != nil {
		return err
	}
	if m.Generation == 0 {
		if m.Object != "" || m.Size != 0 || m.SHA256 != "" {
			return errs.Wrap(module, nil, "manifest of %s: generation 0 must name no object", m.Key)
		}
		return nil
	}
	if m.Object != GenObject(m.Key, m.Generation) {
		return errs.Wrap(module, nil, "manifest of %s: object %q is not generation %d's name", m.Key, m.Object, m.Generation)
	}
	if m.Size < 0 {
		return errs.Wrap(module, nil, "manifest of %s: negative size %d", m.Key, m.Size)
	}
	if b, err := hex.DecodeString(m.SHA256); err != nil || len(b) != 32 || strings.ToLower(m.SHA256) != m.SHA256 {
		return errs.Wrap(module, nil, "manifest of %s: sha256 %q is not 64 lower-case hex digits", m.Key, m.SHA256)
	}
	return nil
}

// Same reports whether two manifests describe the same publish (WrittenAt aside).
func (m Manifest) Same(o Manifest) bool {
	return m.Key == o.Key && m.Generation == o.Generation && m.Epoch == o.Epoch && m.Object == o.Object &&
		m.Size == o.Size && m.SHA256 == o.SHA256
}

// NextManifest is the manifest that publishes generation gen of key at epoch.
//
//	pre:  gen >= 1, d.SHA256 is 64 lower-case hex digits
func NextManifest(key string, gen, epoch uint64, d Digest, now time.Time) Manifest {
	return Manifest{Key: key, Generation: gen, Epoch: epoch, Object: GenObject(key, gen), Size: d.Size,
		SHA256: d.SHA256, WrittenAt: now.UTC()}
}

// CheckPublish decides whether next may replace stored (nil: no manifest yet). Pure; the Store's
// conditional publish makes it hold at the moment of the write.
//
//	post: nil ⇒ next.Epoch >= stored.Epoch and next.Generation > stored.Generation
//	post: otherwise errors.Is(err, ErrStale)
func CheckPublish(stored *Manifest, next Manifest) error {
	if stored == nil {
		return nil
	}
	if stored.Epoch > next.Epoch {
		return errs.Wrap(module, ErrStale, "cannot publish %s at epoch %d: stored epoch is %d", next.Key, next.Epoch, stored.Epoch)
	}
	if next.Generation <= stored.Generation {
		return errs.Wrap(module, ErrStale, "cannot publish %s generation %d: stored generation is already %d",
			next.Key, next.Generation, stored.Generation)
	}
	return nil
}

// CheckStamp decides whether key's manifest may be re-published at epoch, naming the same generation gen
// (fencing's Stamp: the new owner raises the stored epoch before it admits work, so every lower-epoch
// publish fails from then on). stored nil means no manifest; that is allowed only for gen 0. Pure.
//
//	post: nil ⇒ the stamped manifest is stored with Epoch = epoch (or a new generation-0 manifest)
func CheckStamp(stored *Manifest, key string, epoch, gen uint64) error {
	switch {
	case stored == nil && gen != 0:
		return errs.Wrap(module, ErrStale, "cannot stamp %s at generation %d: no manifest exists", key, gen)
	case stored == nil:
		return nil
	case stored.Generation != gen:
		return errs.Wrap(module, ErrStale, "cannot stamp %s at generation %d: stored generation is %d", key, gen, stored.Generation)
	case stored.Epoch > epoch:
		return errs.Wrap(module, ErrStale, "cannot stamp %s at epoch %d: stored epoch is %d", key, epoch, stored.Epoch)
	}
	return nil
}

// Stamp re-publishes key's manifest at epoch, keeping generation gen (see CheckStamp). With no manifest and
// gen 0 it creates a generation-0 manifest. Returns the manifest now stored and its version. Stamping at
// the stored epoch again is a no-op that returns the stored manifest.
func Stamp(ctx context.Context, s Store, key string, epoch, gen uint64, now time.Time) (Manifest, Version, error) {
	stored, v, err := s.ReadManifest(ctx, key)
	var cur *Manifest
	switch {
	case errors.Is(err, ErrNoManifest):
	case err != nil:
		return Manifest{}, NoVersion, errs.Wrap(module, err, "cannot stamp %s: manifest read failed; nothing changed, safe to retry", key)
	default:
		cur = &stored
	}
	if err := CheckStamp(cur, key, epoch, gen); err != nil {
		return Manifest{}, NoVersion, err
	}
	if cur != nil && cur.Epoch == epoch {
		return stored, v, nil
	}
	next := Manifest{Key: key, Epoch: epoch, WrittenAt: now.UTC()}
	if cur != nil {
		next = *cur
		next.Epoch, next.WrittenAt = epoch, now.UTC()
	}
	nv, err := s.PublishManifest(ctx, next, v)
	if err != nil {
		if errors.Is(err, ErrStale) {
			return Manifest{}, NoVersion, errs.Wrap(module, err, "cannot stamp %s at epoch %d: the manifest changed meanwhile; nothing changed", key, epoch)
		}
		return Manifest{}, NoVersion, errs.Wrap(module, err, "cannot stamp %s at epoch %d: publish failed; it may or may not have applied, re-read before retrying", key, epoch)
	}
	return next, nv, nil
}
