// Package fsx holds the file-system helpers the services share. An adapter: callers wrap its errors in their own
// module (errs.Wrap), so it has no error module of its own.
package fsx

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

// TempPrefix starts the name of every temp file WriteAtomic makes, so a directory listing can skip them.
const TempPrefix = ".tmp-"

// WriteAtomic writes data to path so that a crash leaves either the old file or the new one, never a torn one:
// a temp file in path's directory (TempPrefix + random hex, never named after path: a long name may already be
// near the 255-byte limit) created with perm, written and fsynced, renamed over path, then the directory fsynced
// so the rename itself survives a crash. On an error before the rename the temp file is removed.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, TempPrefix+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory: entries created, renamed or removed in it survive a crash.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
