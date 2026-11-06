package tenancy

import (
	"bytes"
	"strings"
	"testing"
)

// betterAuthHashes were made by Better Auth 1.7.7 (npm:better-auth@1.7.7/crypto hashPassword) on 2026-11-06:
// Go must verify them unchanged, so migrated accounts keep their passwords (replace-platform plan D5).
var betterAuthHashes = []struct{ password, hash string }{
	{"E2e-password-Ünïcode", "d93a498cd836d71e7e62c8c0ae02cd6b:8169ad001cdf7d081e2d8dc3d253b8f9c8416bfb4183f99159eb7277ba0eb3934a1346af008f2fb2702a3d7eec6d884cd70e5465f6b791eb487e3410f82f59e0"},
	{"correct horse battery staple", "5f1a4a39142f8680f6acf0644b323600:ed91a4c3469e54b1200519ddfb9efa577390d464bdec2c8a1d8902c44f44edc9d3aadc845e6f77a4d7b049059f8ef4d232cdcd62f0614d5c6695a41ce3ceb033"},
}

func TestVerifiesBetterAuthHashes(t *testing.T) {
	for _, c := range betterAuthHashes {
		if ok, err := verifyPassword(c.hash, c.password); err != nil || !ok {
			t.Errorf("%q: ok %v err %v", c.password, ok, err)
		}
		if ok, _ := verifyPassword(c.hash, c.password+"x"); ok {
			t.Errorf("%q: a wrong password matched", c.password)
		}
	}
	// NFKC: the composed and decomposed forms of a password are the same password (as in Better Auth).
	if ok, _ := verifyPassword(betterAuthHashes[0].hash, "E2e-password-Ünïcode"); !ok {
		t.Error("decomposed form did not match")
	}
}

func TestHashRoundTripAndFormat(t *testing.T) {
	h, err := hashPassword(bytes.NewReader(bytes.Repeat([]byte{7}, 16)), "long enough")
	if err != nil {
		t.Fatal(err)
	}
	salt, key, _ := strings.Cut(h, ":")
	if salt != strings.Repeat("07", 16) || len(key) != 128 {
		t.Fatalf("hash %q", h)
	}
	if ok, _ := verifyPassword(h, "long enough"); !ok {
		t.Fatal("round trip failed")
	}
	for _, bad := range []string{"", "nocolon", "zz:00", strings.Repeat("0", 32) + ":" + strings.Repeat("0", 127), dummyHash[:31] + ":" + dummyHash[33:]} {
		if ok, err := verifyPassword(bad, "long enough"); ok || err != nil {
			t.Errorf("%q: ok %v err %v", bad, ok, err)
		}
	}
	if ok, err := verifyPassword(dummyHash, "anything"); ok || err != nil || len(dummyHash) != 32+1+128 {
		t.Fatalf("dummy hash: ok %v err %v len %d", ok, err, len(dummyHash))
	}
	if _, err := hashPassword(bytes.NewReader(nil), "x"); err == nil {
		t.Fatal("no randomness accepted")
	}
}

func TestPasswordLengths(t *testing.T) {
	for p, ok := range map[string]bool{"1234567": false, "12345678": true, strings.Repeat("é", 128): true, strings.Repeat("é", 129): false, "\xff\xfe\xfd\xfc\xfb\xfa\xf9\xf8": false} {
		if got := checkPassword(p) == nil; got != ok {
			t.Errorf("len %d: ok %v, want %v", len(p), got, ok)
		}
	}
}

func TestLegacyKeyShape(t *testing.T) {
	for k, ok := range map[string]bool{
		"mansio_" + strings.Repeat("A", 42) + "E": true,
		"mansio_" + strings.Repeat("A", 42) + "B": false, // non-canonical last character (Strict)
		"mansio_" + strings.Repeat("A", 42):       false,
		"mansio_" + strings.Repeat("A", 44):       false,
		"other_" + strings.Repeat("A", 42) + "E":  false,
	} {
		if legacyKey(k) != ok {
			t.Errorf("%s: %v", k, !ok)
		}
	}
}
