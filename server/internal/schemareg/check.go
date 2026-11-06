package schemareg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

const module = "schemareg"

// MaxText bounds a schema's text in bytes (the IR request cap; 0006_app_schemas.sql's schemas_body_size).
const MaxText = 64 << 10

var (
	ErrInvalid     = errors.New("schemareg: invalid schema")
	ErrNotFound    = errors.New("schemareg: schema not registered for this app")
	ErrUnavailable = errors.New("schemareg: registry unavailable")
)

// Fingerprint is text's fingerprint: "sha256:" + 64 lowercase hex digits of SHA-256 over its bytes.
func Fingerprint(text []byte) string {
	sum := sha256.Sum256(text)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidFingerprint reports whether s has a fingerprint's form (the schema_fingerprint domain).
func ValidFingerprint(s string) bool {
	const prefix = "sha256:"
	if len(s) != len(prefix)+64 || s[:len(prefix)] != prefix {
		return false
	}
	for _, c := range []byte(s[len(prefix):]) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Check verifies a registration's text against its fingerprint: 2..MaxText bytes of UTF-8 holding one JSON
// object, whose fingerprint is fingerprint. Errors match ErrInvalid and say which rule failed.
func Check(fingerprint string, text []byte) error {
	switch {
	case !ValidFingerprint(fingerprint):
		return fmt.Errorf("%w: fingerprint %q is not sha256: + 64 lowercase hex digits", ErrInvalid, fingerprint)
	case len(text) < 2 || len(text) > MaxText:
		return fmt.Errorf("%w: schema text is %d bytes; want 2..%d", ErrInvalid, len(text), MaxText)
	case !utf8.Valid(text):
		return fmt.Errorf("%w: schema text is not UTF-8", ErrInvalid)
	case text[0] != '{' || !json.Valid(text):
		return fmt.Errorf("%w: schema text is not one JSON object with no leading whitespace", ErrInvalid)
	}
	if got := Fingerprint(text); got != fingerprint {
		return fmt.Errorf("%w: the text's fingerprint is %s, not %s (the fingerprint is SHA-256 of the exact bytes: "+
			"send JSON.stringify(schema), unindented)", ErrInvalid, got, fingerprint)
	}
	return nil
}
