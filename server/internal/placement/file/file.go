//go:build darwin || linux

// Package file is a single-node owner.Placement (the authority that hands out epochs): one small JSON file per
// key in a directory, and every CAS under an exclusive flock on <dir>/.lock. Processes on one host (agentd, an
// operator, a test acting as the control plane) can share one authority. It stands in for the Postgres adapter
// (internal/placement/pg, CP-3) on a single node: it has no replication and no reaper, and Get reads local disk.
//
// It is an adapter: it validates, translates and wraps errors (module "placement"); it does not log.
//
// Why it is linearizable: a CAS reads, compares and replaces the row while holding the lock, and the replace is
// a rename of an fsynced file (then the directory is fsynced). Get reads the file without the lock; a rename is
// atomic, so Get returns the row as of the last CAS that finished before it started, or a later one.
package file

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/fsx"
	"github.com/nickstrad/mansio/server/internal/owner"
)

const (
	module    = "placement"
	rowExt    = ".row"
	tmpPrefix = fsx.TempPrefix
	lockName  = ".lock"
	rowFormat = 1
)

// errInvariant marks a CAS that would break the authority's invariants. It is deliberately neither
// owner.ErrConflict nor owner.ErrNotFound: the caller has a bug, nobody else won (art-of-postgresql plan PG-03).
var errInvariant = errors.New("placement: CAS breaks the authority's invariants")

// Placement implements owner.Placement over a directory. Safe for concurrent use, also across processes on
// one host.
type Placement struct {
	dir  string
	poll time.Duration // lock retry interval
}

var _ owner.Placement = (*Placement)(nil)

type row struct {
	V     int    `json:"v"`
	Key   string `json:"key"`
	Owner string `json:"owner"`
	Epoch uint64 `json:"epoch"`
	Gen   uint64 `json:"gen"`
	State string `json:"state"`
}

// Open creates dir if needed and returns the authority stored in it.
func Open(dir string) (*Placement, error) {
	if dir == "" {
		return nil, errs.Wrap(module, nil, "cannot open placement: no directory given")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.Wrap(module, err, "cannot create placement directory %s", dir)
	}
	return &Placement{dir: dir, poll: time.Millisecond}, nil
}

func (p *Placement) path(key string) string {
	return filepath.Join(p.dir, hex.EncodeToString([]byte(key))+rowExt)
}

// Get implements owner.Placement.
func (p *Placement) Get(ctx context.Context, key string) (owner.Place, error) {
	if !owner.ValidKey(key) {
		return owner.Place{}, errs.Wrap(module, owner.ErrInvalid, "cannot read placement of %q: key must be 1..%d bytes", key, owner.MaxKeyLen)
	}
	if err := ctx.Err(); err != nil {
		return owner.Place{}, errs.Wrap(module, err, "cannot read placement of %q", key)
	}
	pl, found, err := p.read(key)
	switch {
	case err != nil:
		return owner.Place{}, errs.Wrap(module, err, "cannot read placement of %q; safe to retry", key).With("dir", p.dir)
	case !found:
		return owner.Place{}, errs.Wrap(module, owner.ErrNotFound, "no placement for %q", key)
	}
	return pl, nil
}

// CAS implements owner.Placement. It waits for the lock until ctx ends.
func (p *Placement) CAS(ctx context.Context, key string, expect, next owner.Place) error {
	if !owner.ValidKey(key) {
		return errs.Wrap(module, owner.ErrInvalid, "cannot set placement of %q: key must be 1..%d bytes", key, owner.MaxKeyLen)
	}
	if err := checkPlace(next); err != nil {
		return errs.Wrap(module, err, "cannot set placement of %q to %+v; nothing changed", key, next)
	}
	unlock, err := p.lock(ctx)
	if err != nil {
		return errs.Wrap(module, err, "cannot set placement of %q: lock not taken; nothing changed, safe to retry", key).With("dir", p.dir)
	}
	defer unlock()
	cur, _, err := p.read(key) // a missing row reads as the zero Place: "create"
	if err != nil {
		return errs.Wrap(module, err, "cannot set placement of %q: current row unreadable; nothing changed", key).With("dir", p.dir)
	}
	if cur != expect {
		return errs.Wrap(module, owner.ErrConflict, "cannot set placement of %q: row is %+v, expected %+v; nothing changed", key, cur, expect)
	}
	if err := checkStep(cur, next); err != nil {
		return errs.Wrap(module, err, "cannot set placement of %q from %+v to %+v; nothing changed (a caller bug)", key, cur, next)
	}
	if err := ctx.Err(); err != nil {
		return errs.Wrap(module, err, "cannot set placement of %q: cancelled; nothing changed, safe to retry", key)
	}
	data, err := json.Marshal(row{V: rowFormat, Key: key, Owner: next.Owner, Epoch: uint64(next.Epoch), Gen: uint64(next.Gen), State: next.State.String()})
	if err != nil {
		return errs.Wrap(module, err, "cannot encode placement of %q; nothing changed", key)
	}
	if err := fsx.WriteAtomic(p.path(key), data, 0o644); err != nil {
		return errs.Wrap(module, err, "cannot write placement of %q; the old row or the new one is in place, re-read before retrying", key).With("dir", p.dir)
	}
	return nil
}

// checkPlace validates a row to be written.
func checkPlace(pl owner.Place) error {
	switch {
	case pl.Owner == "":
		return fmt.Errorf("%w: empty owner", errInvariant)
	case pl.Epoch == 0:
		return fmt.Errorf("%w: epoch 0", errInvariant)
	case pl.State != owner.PlaceActive && pl.State != owner.PlaceMoving:
		return fmt.Errorf("%w: state %d", errInvariant, pl.State)
	}
	return nil
}

// checkStep enforces the authority's invariants (owner/ports.go): epochs never decrease, and the owner changes
// only together with an epoch increase.
func checkStep(cur, next owner.Place) error {
	switch {
	case next.Epoch < cur.Epoch:
		return fmt.Errorf("%w: epoch %d → %d goes down", errInvariant, cur.Epoch, next.Epoch)
	case cur.Epoch != 0 && next.Owner != cur.Owner && next.Epoch <= cur.Epoch:
		return fmt.Errorf("%w: owner %q → %q without an epoch increase", errInvariant, cur.Owner, next.Owner)
	}
	return nil
}

// read returns key's row; found is false if there is none.
func (p *Placement) read(key string) (pl owner.Place, found bool, err error) {
	data, err := os.ReadFile(p.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return owner.Place{}, false, nil
	}
	if err != nil {
		return owner.Place{}, false, err
	}
	var r row
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return owner.Place{}, false, fmt.Errorf("%w: row file: %v", owner.ErrInvalid, err)
	}
	if r.V != rowFormat || r.Key != key {
		return owner.Place{}, false, fmt.Errorf("%w: row file holds format %d, key %q (want %d, %q)", owner.ErrInvalid, r.V, r.Key, rowFormat, key)
	}
	pl = owner.Place{Owner: r.Owner, Epoch: owner.Epoch(r.Epoch), Gen: owner.Generation(r.Gen)}
	for _, st := range []owner.PlaceState{owner.PlaceActive, owner.PlaceMoving} {
		if r.State == st.String() {
			pl.State = st
		}
	}
	if err := checkPlace(pl); err != nil { // an unknown state stays 0 and fails here
		return owner.Place{}, false, fmt.Errorf("%w: stored row: %w", owner.ErrInvalid, err)
	}
	return pl, true, nil
}

// lock takes the directory's exclusive flock, polling until ctx ends (a blocking flock could not honor ctx).
// The lock is per open file, so it also excludes other goroutines of this process.
func (p *Placement) lock(ctx context.Context) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(p.dir, lockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(p.poll):
		}
	}
}
