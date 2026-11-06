package owner_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

func TestFileStoreContract(t *testing.T) {
	ownertest.RunLocalStoreSuite(t, func(t *testing.T) (owner.LocalStore, func() owner.LocalStore) {
		dir := filepath.Join(t.TempDir(), "owner")
		s, err := owner.NewFileStore(dir)
		must(t, err)
		return s, func() owner.LocalStore {
			s, err := owner.NewFileStore(dir)
			must(t, err)
			return s
		}
	})
}

func fileName(key string) string { return hex.EncodeToString([]byte(key)) + ".rec" }

// A Put that crashed before its rename leaves a temp file: Load ignores (and removes) it and keeps the old
// record, which is the "old or new, never torn" half of the contract we can test without pulling power.
func TestFileStoreCrashedPutLeavesOldRecord(t *testing.T) {
	dir := t.TempDir()
	s, err := owner.NewFileStore(dir)
	must(t, err)
	old := owner.Record{Key: "k", State: owner.Active, Epoch: 1}
	must(t, s.Put(ctx, old))
	tmp := filepath.Join(dir, ".tmp-abc")
	must(t, os.WriteFile(tmp, []byte(`{"v":1,"key":"k","sta`), 0o644)) // torn write of the next record
	recs, err := s.Load(ctx)
	must(t, err)
	if len(recs) != 1 || recs[0] != old {
		t.Fatalf("Load = %+v, want only %+v", recs, old)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("leftover temp file not removed")
	}
}

func TestFileStoreFailsClosedOnBadRecords(t *testing.T) {
	for name, tc := range map[string]struct{ file, body string }{
		"torn json":       {fileName("k"), `{"v":1,"key":"k"`},
		"unknown field":   {fileName("k"), `{"v":1,"key":"k","state":"active","epoch":1,"gen":0,"x":1}`},
		"future format":   {fileName("k"), `{"v":2,"key":"k","state":"active","epoch":1,"gen":0}`},
		"unknown state":   {fileName("k"), `{"v":1,"key":"k","state":"moving","epoch":1,"gen":0}`},
		"epoch 0":         {fileName("k"), `{"v":1,"key":"k","state":"active","epoch":0,"gen":0}`},
		"active sup":      {fileName("k"), `{"v":1,"key":"k","state":"active","epoch":1,"gen":0,"superseded":true}`},
		"name ≠ key":      {fileName("other"), `{"v":1,"key":"k","state":"active","epoch":1,"gen":0}`},
		"unreadable file": {fileName("k"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, tc.file)
			if name == "unreadable file" {
				must(t, os.Mkdir(p, 0o755)) // a directory where a record should be
			} else {
				must(t, os.WriteFile(p, []byte(tc.body), 0o644))
			}
			s, err := owner.NewFileStore(dir)
			must(t, err)
			if _, err := s.Load(ctx); err == nil {
				t.Fatal("Load accepted a bad record")
			}
			// The table refuses to open on it: nothing is admitted.
			if _, err := owner.Open(ctx, owner.Config{Self: "a", Store: s, Placement: ownertest.NewPlacement(),
				WriteBack: &ownertest.WriteBack{Objects: ownertest.NewObjects(), Disk: ownertest.NewDisk()}}); err == nil {
				t.Fatal("Open accepted a bad record")
			}
		})
	}
}

func TestFileStoreErrors(t *testing.T) {
	if _, err := owner.NewFileStore(""); err == nil {
		t.Fatal("empty dir accepted")
	}
	dir := t.TempDir()
	s, err := owner.NewFileStore(dir)
	must(t, err)
	mustIs(t, s.Delete(ctx, ""), owner.ErrInvalid)
	must(t, os.WriteFile(filepath.Join(dir, "README"), nil, 0o644)) // unrelated files are ignored
	if recs, err := s.Load(ctx); err != nil || len(recs) != 0 {
		t.Fatalf("Load = %v %v", recs, err)
	}
	gone := &owner.FileStore{Dir: filepath.Join(dir, "missing")}
	if _, err := gone.Load(ctx); err == nil {
		t.Fatal("Load of a missing dir succeeded")
	}
	if err := gone.Put(ctx, owner.Record{Key: "k", State: owner.Active, Epoch: 1}); err == nil {
		t.Fatal("Put into a missing dir succeeded")
	}
	cctx, cancel := contextCanceled()
	defer cancel()
	if err := s.Put(cctx, owner.Record{Key: "k", State: owner.Active, Epoch: 1}); err == nil {
		t.Fatal("Put with a cancelled context succeeded")
	}
}
