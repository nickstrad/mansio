package locator

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// API keys: "mansio_" + id (16 hex digits, the row's primary key) + "_" + secret (32 random bytes, base64url).
// The store keeps only SHA-256(secret): the secret has 256 bits of entropy, so a slow hash adds nothing, and a
// leaked table yields no usable key.
const (
	apiKeyPrefix = "mansio_"
	idLen        = 16
	secretLen    = 32
)

var secretEnc = base64.RawURLEncoding.Strict()

// NewAPIKey makes a key from rand: the key to hand out once, its id and the hash to store.
func NewAPIKey(rand io.Reader) (key, id string, hash [32]byte, err error) {
	var b [idLen/2 + secretLen]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return "", "", hash, fmt.Errorf("api key: no randomness: %w", err)
	}
	id = hex.EncodeToString(b[:idLen/2])
	secret := b[idLen/2:]
	return apiKeyPrefix + id + "_" + secretEnc.EncodeToString(secret), id, sha256.Sum256(secret), nil
}

// ParseAPIKey splits a key into its id and the hash of its secret; ok is false for anything malformed.
func ParseAPIKey(key string) (id string, hash [32]byte, ok bool) {
	rest, found := strings.CutPrefix(key, apiKeyPrefix)
	if !found || len(rest) < idLen+1 || rest[idLen] != '_' {
		return "", hash, false
	}
	id = rest[:idLen]
	if !ValidKeyID(id) {
		return "", hash, false
	}
	secret, err := secretEnc.DecodeString(rest[idLen+1:])
	if err != nil || len(secret) != secretLen {
		return "", hash, false
	}
	return id, sha256.Sum256(secret), true
}

// ValidKeyID reports whether id is 16 lowercase hex digits (the api_keys.key_id check).
func ValidKeyID(id string) bool {
	if len(id) != idLen {
		return false
	}
	for i := range len(id) {
		if c := id[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
