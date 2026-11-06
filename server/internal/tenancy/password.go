package tenancy

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
	"golang.org/x/text/unicode/norm"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// Password hashes are Better Auth 1.7's (better-auth/crypto hashPassword, verified against a real hash in
// password_test.go): "<salt>:<key>", where salt is 16 random bytes in lowercase hex and key is
// scrypt(NFKC(password), salt's hex TEXT as the salt bytes, N=16384, r=16, p=1, 64 bytes) in lowercase hex.
// So accounts migrated from Better Auth sign in with their passwords unchanged (replace-platform plan D5).
const (
	scryptN, scryptR, scryptP, scryptKeyLen = 16384, 16, 1, 64
	saltLen                                 = 16
	minPassword, maxPassword                = 8, 128 // Better Auth's defaults, in characters
)

// dummyHash is verified against when a sign-in names no user, so an unknown email costs as much as a wrong
// password (no user enumeration by timing). Its password is unknown (random at generation).
const dummyHash = "1d4f8c0a6b2e9f3c7a5d1e8b4c6f2a90:" +
	"5c1b8e2f4a7d9c3e6b0f1a2d5e8c4b7f9a3d6e1c8b5f2a4d7e9c0b3f6a1d8e5c" +
	"2b4f7a9d1e3c6b8f0a5d2e7c4b9f1a6d3e8c5b0f7a2d4e9c1b6f3a8d5e0c7b2f"

// hashPassword returns password's Better Auth hash, salted from rand.
func hashPassword(rand io.Reader, password string) (string, error) {
	var salt [saltLen]byte
	if _, err := io.ReadFull(rand, salt[:]); err != nil {
		return "", errs.Wrap(module, err, "no randomness for a password salt; nothing changed")
	}
	s := hex.EncodeToString(salt[:])
	key, err := derive(password, s)
	if err != nil {
		return "", err
	}
	return s + ":" + hex.EncodeToString(key), nil
}

// verifyPassword reports whether password matches hash (constant-time). A malformed hash never matches.
func verifyPassword(hash, password string) (bool, error) {
	salt, want, ok := strings.Cut(hash, ":")
	key, err := hex.DecodeString(want)
	if !ok || err != nil || len(key) != scryptKeyLen || len(salt) != 2*saltLen {
		return false, nil
	}
	got, err := derive(password, salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

func derive(password, salt string) ([]byte, error) {
	key, err := scrypt.Key([]byte(norm.NFKC.String(password)), []byte(salt), scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil { // only for invalid parameters: constants
		return nil, errs.Wrap(module, err, "scrypt refused its parameters (a bug)")
	}
	return key, nil
}

// checkPassword validates a new password's length (Better Auth's 8..128 characters).
func checkPassword(p string) error {
	if n := utf8.RuneCountInString(p); n < minPassword || n > maxPassword || !utf8.ValidString(p) {
		return errs.Wrap(module, ErrInvalid, "password must be %d..%d characters", minPassword, maxPassword)
	}
	return nil
}

// hashing runs fn in a hash slot (the bulkhead: scrypt holds 32 MiB and tens of ms of CPU each). It waits for a
// slot at most until ctx ends; then nothing ran.
func (s *Service) hashing(ctx context.Context, fn func() error) error {
	select {
	case s.hash <- struct{}{}:
	case <-ctx.Done():
		return errs.Wrap(module, ErrUnavailable, "every password-hash slot is busy until the deadline; nothing changed, retry later")
	}
	defer func() { <-s.hash }()
	return fn()
}
