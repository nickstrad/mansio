package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/ticket"
)

func TestKeyDirRotation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	pol := ticket.RotationPolicy{Rotate: time.Hour, Activate: time.Minute, Overlap: 3 * time.Minute}
	k := &keyDir{dir: dir, policy: pol, signer: &ticket.Signer{}}
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	files := func() []string {
		names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for i, n := range names {
			names[i] = strings.TrimSuffix(filepath.Base(n), ".json")
		}
		return names
	}
	published := func() []string {
		var out []string
		for _, e := range k.signer.Public().Keys {
			out = append(out, e.KID)
		}
		return out
	}

	created, _, err := k.rotate(t0)
	if err != nil || created == "" || k.signer.KID() != created || !slices.Equal(files(), []string{created}) {
		t.Fatalf("bootstrap: %q %v, files %v", created, err, files())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dir, created+".json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	first := created

	// A restart reloads the same key: no new key, same signer.
	k2 := &keyDir{dir: dir, policy: pol, signer: &ticket.Signer{}}
	if c, _, err := k2.rotate(t0.Add(time.Minute)); err != nil || c != "" || k2.signer.KID() != first {
		t.Fatalf("reload: %q %v %s", c, err, k2.signer.KID())
	}

	// At Rotate a second key is created and published, but the first still signs until Activate.
	second, _, err := k.rotate(t0.Add(time.Hour))
	if err != nil || second == "" || k.signer.KID() != first || len(published()) != 2 {
		t.Fatalf("rotate: %q %v signer %s published %v", second, err, k.signer.KID(), published())
	}
	if _, _, err := k.rotate(t0.Add(time.Hour + time.Minute)); err != nil || k.signer.KID() != second {
		t.Fatalf("activate: %v signer %s", err, k.signer.KID())
	}
	// Overlap after the second began signing, the first is deleted.
	_, removed, err := k.rotate(t0.Add(time.Hour + 4*time.Minute))
	if err != nil || !slices.Equal(removed, []string{first}) || !slices.Equal(files(), []string{second}) || !slices.Equal(published(), []string{second}) {
		t.Fatalf("retire: removed %v %v files %v published %v", removed, err, files(), published())
	}
}

func TestKeyDirRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	k := &keyDir{dir: dir, policy: ticket.RotationPolicy{Rotate: time.Hour}, signer: &ticket.Signer{}}
	os.Chmod(dir, 0o755)
	if _, _, err := k.rotate(time.Now()); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("group-readable dir: %v", err)
	}
	os.Chmod(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "k1.json"), []byte(`{"kid":"k1","seed":"x","created":"2026-11-05T00:00:00Z"}`), 0o600)
	if _, _, err := k.rotate(time.Now()); err == nil || !strings.Contains(err.Error(), "bad seed") {
		t.Fatalf("bad seed: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "k1.json"), []byte(`{"kid":"k2","seed":"x","created":"2026-11-05T00:00:00Z"}`), 0o600)
	if _, _, err := k.rotate(time.Now()); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("kid not the file name: %v", err)
	}
	os.Chmod(filepath.Join(dir, "k1.json"), 0o644)
	if _, _, err := k.rotate(time.Now()); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("readable key file: %v", err)
	}
}
