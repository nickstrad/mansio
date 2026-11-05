// Package durable is the durable copy of one database in object storage: immutable generations plus a
// manifest that names the current one (contract 1 in shared_state.md, 2026-11-05).
//
// Layout per database key (v2, object-storage guide K1–K3), under KeyPrefix(key) = dbs/<h2>/<esc>/ where
// h2 is the first 2 hex digits of sha256(key) and esc is the key path-escaped ("/" becomes "%2F"), so no
// key's prefix contains another's:
//
//	dbs/<h2>/<esc>/gen/<generation 20d>-e<epoch 20d>.db   immutable SQLite snapshot, uploaded at epoch,
//	                                                      created once, never overwritten
//	dbs/<h2>/<esc>/manifest.json   {key, generation, epoch, object, size, sha256, written_at}
//
// The epoch in a generation's name (guide K4, OS-05) means two owners can never collide on a name, and an
// orphan names the epoch that wrote it. Each generation also carries user metadata (sha256, epoch,
// generation, key, format: guide I4), enough to rebuild a manifest from the object alone. A manifest names
// an object uploaded at an epoch no greater than its own: a Stamp raises the manifest's epoch without
// renaming the object.
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	Object     string    `json:"object"` // GenObject(Key, Generation, e) with e <= Epoch; "" for generation 0
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"` // lower-case hex; "" for generation 0
	WrittenAt  time.Time `json:"written_at"`
}

// Digest is what a snapshot file holds.
type Digest struct {
	Size   int64
	SHA256 string // lower-case hex
}

// Format is the format metadata of a generation that holds one SQLite database file.
const Format = "sqlite3-v1"

// GenInfo is what the store says about one stored generation object (its HEAD): its size and the user
// metadata PutGeneration recorded (guide I4). Fields are zero when the object lacks that metadata.
type GenInfo struct {
	Size       int64
	SHA256     string // lower-case hex
	Key        string
	Generation uint64
	Epoch      uint64 // the epoch it was uploaded at (also in its name)
	Format     string
}

// Holds reports whether the object holds exactly the snapshot d (same size and recorded sha256).
func (g GenInfo) Holds(d Digest) bool {
	return g.SHA256 != "" && g.Size == d.Size && g.SHA256 == d.SHA256
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

	// PutGeneration uploads the file at path as m.Object, only if that object does not exist, with user
	// metadata sha256, epoch, generation, key and format (GenInfo).
	//
	//	pre:  m is NextManifest(key, gen, epoch, d, _) for the file's digest d, with gen >= 1
	//	post: the object already existed ⇒ errors.Is(err, ErrExists), nothing overwritten
	//	post: err == nil ⇒ the object holds the file's bytes (size verified; sha256 sent for the store to check)
	//	post: other err ⇒ the object may or may not exist; it is not referenced by any manifest
	PutGeneration(ctx context.Context, m Manifest, path string) error

	// StatGeneration returns the size and recorded metadata of the stored object named object (a GenObject
	// name). It is the re-read that resolves an ambiguous PutGeneration (object-storage guide R3).
	//
	//	post: missing object ⇒ errors.Is(err, ErrNotFound)
	//	post: err == nil ⇒ info describes the object as PutGeneration uploaded it
	StatGeneration(ctx context.Context, object string) (GenInfo, error)

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

// MaxKeyLen is the longest database key in bytes, for every package (internal/owner uses it too). An
// escaped key is at most 3×120 bytes, so every object name stays far below S3's 1024-byte limit.
const MaxKeyLen = 120

// ValidateKey checks a database key at the edge: 1 to MaxKeyLen bytes, "/"-separated segments that are
// not empty, "." or "..", with no leading or trailing "/", backslash or NUL. Keys are escaped in object
// names (KeyPrefix), so these rules keep keys readable; they are not what keeps prefixes apart.
func ValidateKey(key string) error {
	if key == "" || len(key) > MaxKeyLen {
		return errs.Wrap(module, nil, "invalid database key %q: must be 1-%d bytes", key, MaxKeyLen)
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

// KeyPrefix is the one prefix every object of key lives under: "dbs/<h2>/<esc>/" (see the package doc).
// Escaping makes the key one path segment, so for distinct keys a and b neither KeyPrefix is a prefix of
// the other (FuzzKeyPrefixDisjoint): GC, drop and inventory by prefix never reach another database.
func KeyPrefix(key string) string {
	h := sha256.Sum256([]byte(key))
	return "dbs/" + hex.EncodeToString(h[:1]) + "/" + url.PathEscape(key) + "/"
}

// GenObject is the object name of key's generation gen uploaded at epoch.
func GenObject(key string, gen, epoch uint64) string {
	return fmt.Sprintf("%sgen/%020d-e%020d.db", KeyPrefix(key), gen, epoch)
}

// objectEpoch returns the epoch in object if it is a GenObject name of key's generation gen.
func objectEpoch(key string, gen uint64, object string) (uint64, bool) {
	const digits = 20
	rest, ok := strings.CutPrefix(object, fmt.Sprintf("%sgen/%020d-e", KeyPrefix(key), gen))
	if !ok || len(rest) != digits+len(".db") || !strings.HasSuffix(rest, ".db") {
		return 0, false
	}
	var epoch uint64
	for _, c := range rest[:digits] {
		if c < '0' || c > '9' {
			return 0, false
		}
		next := epoch*10 + uint64(c-'0')
		if next/10 != epoch { // overflow
			return 0, false
		}
		epoch = next
	}
	return epoch, true
}

// ManifestObject is the object name of key's manifest.
func ManifestObject(key string) string { return KeyPrefix(key) + "manifest.json" }

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
	if e, ok := objectEpoch(m.Key, m.Generation, m.Object); !ok {
		return errs.Wrap(module, nil, "manifest of %s: object %q is not generation %d's name", m.Key, m.Object, m.Generation)
	} else if e > m.Epoch {
		return errs.Wrap(module, nil, "manifest of %s at epoch %d names %q, uploaded at the later epoch %d", m.Key, m.Epoch, m.Object, e)
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

// Digest is the snapshot m's generation holds.
func (m Manifest) Digest() Digest { return Digest{Size: m.Size, SHA256: m.SHA256} }

// ObjectInfo is the metadata PutGeneration records with m's object.
//
//	pre: m.Object == GenObject(m.Key, m.Generation, m.Epoch) (m is a NextManifest)
func (m Manifest) ObjectInfo() GenInfo {
	return GenInfo{Size: m.Size, SHA256: m.SHA256, Key: m.Key, Generation: m.Generation, Epoch: m.Epoch, Format: Format}
}

// NextManifest is the manifest that publishes generation gen of key, uploaded and published at epoch.
//
//	pre:  gen >= 1, d.SHA256 is 64 lower-case hex digits
func NextManifest(key string, gen, epoch uint64, d Digest, now time.Time) Manifest {
	return Manifest{Key: key, Generation: gen, Epoch: epoch, Object: GenObject(key, gen, epoch), Size: d.Size,
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

// Stamped is the manifest a stamp publishes: cur (nil: a new generation-0 manifest) at epoch.
//
//	pre: CheckStamp(cur, key, epoch, gen) == nil
func Stamped(cur *Manifest, key string, epoch uint64, now time.Time) Manifest {
	next := Manifest{Key: key}
	if cur != nil {
		next = *cur
	}
	next.Epoch, next.WrittenAt = epoch, now.UTC()
	return next
}

// Stamp re-publishes key's manifest at epoch, keeping generation gen (see CheckStamp). With no manifest and
// gen 0 it creates a generation-0 manifest. Returns the manifest now stored and its version. Stamping at
// the stored epoch again is a no-op that returns the stored manifest.
//
// A failed publish is ambiguous (a lost reply, or a 412 that a re-send of our own applied PUT caused), so
// Stamp re-reads before it reports failure (object-storage guide R3): if the stored manifest is the one it
// stamped, the stamp applied and Stamp succeeds.
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
	next := Stamped(cur, key, epoch, now)
	nv, err := s.PublishManifest(ctx, next, v)
	if err != nil {
		if got, gv, rerr := s.ReadManifest(ctx, key); rerr == nil && got.Same(next) {
			return got, gv, nil
		}
		if errors.Is(err, ErrStale) {
			return Manifest{}, NoVersion, errs.Wrap(module, err, "cannot stamp %s at epoch %d: the manifest changed meanwhile; nothing changed", key, epoch)
		}
		return Manifest{}, NoVersion, errs.Wrap(module, err, "cannot stamp %s at epoch %d: publish failed; it may or may not have applied, re-read before retrying", key, epoch)
	}
	return next, nv, nil
}
