package owner

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/fsx"
)

// FileStore is the LocalStore adapter: one small JSON file per key in Dir, named by the key in hex (so any
// key is a safe file name, also on case-insensitive file systems).
//
// Put writes .tmp-<random>, fsyncs it, renames it over <hex>.rec, then fsyncs Dir, so a crash leaves
// either the old record or the new one. Load ignores and removes leftover temp files, so call it only before
// any Put (Open does).
type FileStore struct {
	Dir string
}

const (
	recExt    = ".rec"
	tmpMarker = fsx.TempPrefix // temp files are ".tmp-<random>"
	recFormat = 1
)

type fileRecord struct {
	V     int    `json:"v"`
	Key   string `json:"key"`
	State string `json:"state"`
	Epoch uint64 `json:"epoch"`
	Gen   uint64 `json:"gen"`
	Sup   bool   `json:"superseded,omitempty"`
}

// NewFileStore creates dir if needed.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errs.Wrap(module, ErrInvalid, "cannot open ownership store: no directory given")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.Wrap(module, err, "cannot create ownership store directory %s", dir)
	}
	return &FileStore{Dir: dir}, nil
}

func (s *FileStore) path(key string) string {
	return filepath.Join(s.Dir, hex.EncodeToString([]byte(key))+recExt)
}

// Put implements LocalStore.
func (s *FileStore) Put(ctx context.Context, r Record) error {
	if err := r.Validate(); err != nil {
		return errs.Wrap(module, err, "cannot store ownership of %q; nothing changed", r.Key)
	}
	data, err := json.Marshal(fileRecord{V: recFormat, Key: r.Key, State: r.State.String(), Epoch: uint64(r.Epoch), Gen: uint64(r.Gen), Sup: r.Superseded})
	if err != nil {
		return errs.Wrap(module, err, "cannot encode ownership of %q; nothing changed", r.Key)
	}
	if err := ctx.Err(); err != nil {
		return errs.Wrap(module, err, "cannot store ownership of %q; nothing changed, safe to retry", r.Key)
	}
	if err := fsx.WriteAtomic(s.path(r.Key), data, 0o644); err != nil {
		return errs.Wrap(module, err, "cannot store ownership of %q; old record kept or replaced, safe to retry", r.Key).With("dir", s.Dir)
	}
	return nil
}

// Delete implements LocalStore.
func (s *FileStore) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return errs.Wrap(module, ErrInvalid, "cannot delete ownership of %q: bad key", key)
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errs.Wrap(module, err, "cannot delete ownership of %q; safe to retry", key)
	}
	if err := fsx.SyncDir(s.Dir); err != nil {
		return errs.Wrap(module, err, "cannot delete ownership of %q durably; safe to retry", key)
	}
	return nil
}

// Load implements LocalStore.
func (s *FileStore) Load(ctx context.Context) ([]Record, error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot read ownership store %s", s.Dir)
	}
	var out []Record
	for _, e := range ents {
		name := e.Name()
		if strings.Contains(name, tmpMarker) {
			_ = os.Remove(filepath.Join(s.Dir, name)) // a Put that crashed before its rename
			continue
		}
		if !strings.HasSuffix(name, recExt) {
			continue
		}
		r, err := readRecord(filepath.Join(s.Dir, name))
		if err != nil {
			return nil, errs.Wrap(module, err, "cannot load ownership record %s; refusing to start with it (fail closed)", name).With("dir", s.Dir)
		}
		if hex.EncodeToString([]byte(r.Key))+recExt != name {
			return nil, errs.Wrap(module, ErrInvalid, "ownership record %s holds key %q; refusing to start with it (fail closed)", name, r.Key)
		}
		out = append(out, r)
	}
	return out, nil
}

func readRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var fr fileRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fr); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if fr.V != recFormat {
		return Record{}, fmt.Errorf("%w: format %d, want %d", ErrInvalid, fr.V, recFormat)
	}
	r := Record{Key: fr.Key, Epoch: Epoch(fr.Epoch), Gen: Generation(fr.Gen), Superseded: fr.Sup}
	switch fr.State {
	case Active.String():
		r.State = Active
	case Fenced.String():
		r.State = Fenced
	default:
		return Record{}, fmt.Errorf("%w: state %q", ErrInvalid, fr.State)
	}
	return r, r.Validate()
}
