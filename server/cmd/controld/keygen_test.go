package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/ticket"
)

// keygen seeds a ring controld serves from: the bootstrap key signs at once, and the -public file verifies
// what it signs. A second keygen on the same ring is refused and leaves the ring as it was.
func TestKeygenSeedsRing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ticket-keys")
	pub := filepath.Join(t.TempDir(), "keys.json")
	var out, errOut bytes.Buffer
	if code := realMain(t.Context(), []string{"keygen", "-dir", dir, "-public", pub}, &out, &errOut); code != exitOK {
		t.Fatalf("keygen exit %d: %s", code, errOut.String())
	}
	kid := strings.TrimSpace(out.String())
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("ring dir: %v %v", fi, err)
	}

	pol := ticket.RotationPolicy{Rotate: time.Hour, Activate: 5 * time.Minute, Overlap: 3 * time.Minute}
	k := &keyDir{dir: dir, policy: pol, signer: &ticket.Signer{}}
	if created, _, err := k.rotate(time.Now()); err != nil || created != "" || k.signer.KID() != kid {
		t.Fatalf("controld on the seeded ring: created %q err %v signer %q, want %q", created, err, k.signer.KID(), kid)
	}
	f, err := os.Open(pub)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	keys, err := ticket.ParsePublicKeys(f)
	if err != nil || len(keys) != 1 || keys[kid] == nil {
		t.Fatalf("public key set: %v %v", keys, err)
	}
	now := time.Now()
	tok, err := k.signer.Mint(ticket.Claims{TID: "t", DBID: "db1", VM: "vm-a", Epoch: 1, Sub: "s",
		Perms: ticket.Read, IAT: now.Unix(), EXP: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (ticket.Verifier{Keys: ticket.NewKeyRing(keys), VM: "vm-a"}).Verify(tok, now, ticket.Want{DB: "db1", Perm: ticket.Read}); err != nil {
		t.Fatalf("the -public set does not verify the ring's ticket: %v", err)
	}

	errOut.Reset()
	if code := realMain(t.Context(), []string{"keygen", "-dir", dir}, &out, &errOut); code != exitError || !strings.Contains(errOut.String(), "already holds") {
		t.Fatalf("second keygen: exit %d %s", code, errOut.String())
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(names) != 1 {
		t.Fatalf("ring changed by a refused keygen: %v", names)
	}
	if code := realMain(t.Context(), []string{"keygen"}, &out, &errOut); code != exitUsage {
		t.Fatalf("no -dir: exit %d", code)
	}
}
