package schemareg

import (
	"errors"
	"strings"
	"testing"
)

func TestFingerprint(t *testing.T) {
	// sha256("{}"), the smallest schema text.
	const want = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if got := Fingerprint([]byte("{}")); got != want || !ValidFingerprint(got) {
		t.Fatalf("Fingerprint({}) = %s", got)
	}
	hex64 := strings.Repeat("a", 64)
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"sha256:" + hex64, true},
		{"sha256:" + strings.Repeat("0123456789abcdef", 4), true},
		{"sha256:" + hex64[:63], false},
		{"sha256:" + hex64 + "a", false},
		{"sha256:" + strings.ToUpper(hex64), false},
		{"sha256:" + hex64[:63] + "g", false},
		{"sha512:" + hex64, false},
		{hex64, false},
		{"", false},
	} {
		if got := ValidFingerprint(tc.s); got != tc.want {
			t.Errorf("ValidFingerprint(%q) = %v", tc.s, got)
		}
	}
}

func TestCheck(t *testing.T) {
	big := `{"t":"` + strings.Repeat("x", MaxText-8) + `"}` // MaxText bytes exactly
	if len(big) != MaxText {
		t.Fatalf("fixture is %d bytes", len(big))
	}
	for _, tc := range []struct {
		name string
		text string
		fp   string // "" = Fingerprint(text)
		ok   bool
	}{
		{"smallest object (on point)", `{}`, "", true},
		{"a schema", `{"games":{"fields":{"x":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]}}`, "", true},
		{"MaxText bytes (on point)", big, "", true},
		{"MaxText+1 bytes (off point)", big + " ", "", false},
		{"one byte", `{`, "", false},
		{"empty", ``, "", false},
		{"leading whitespace", ` {}`, "", false},
		{"an array", `[]`, "", false},
		{"trailing text", `{} x`, "", false},
		{"not UTF-8", "{\"a\":\"\xff\"}", "", false},
		{"another text's fingerprint", `{"a":1}`, Fingerprint([]byte(`{"a": 1}`)), false},
		{"malformed fingerprint", `{}`, "sha256:xyz", false},
	} {
		fp := tc.fp
		if fp == "" {
			fp = Fingerprint([]byte(tc.text))
		}
		err := Check(fp, []byte(tc.text))
		if (err == nil) != tc.ok || err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Check = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

func TestCacheBounds(t *testing.T) {
	c := newCache(2, 10)
	k := func(s string) cacheKey { return cacheKey{"app", s} }
	c.put(k("a"), "aaaa")
	c.put(k("b"), "bbbb")
	c.get(k("a"))       // a is now the most recent
	c.put(k("c"), "cc") // over 2 entries: evicts b
	if _, ok := c.get(k("b")); ok || c.len() != 2 || c.bytes != 6 {
		t.Fatalf("entries bound: len %d bytes %d", c.len(), c.bytes)
	}
	c.put(k("d"), "dddddd") // 12 bytes > 10: evicts the least recent (a), then fits
	if _, ok := c.get(k("a")); ok || c.bytes != 8 {
		t.Fatalf("bytes bound: len %d bytes %d", c.len(), c.bytes)
	}
	c.put(k("e"), strings.Repeat("e", 11)) // larger than the whole budget: not cached
	if _, ok := c.get(k("e")); ok {
		t.Fatal("an oversized text was cached")
	}
	c.put(k("d"), "dddddd") // a refresh adds nothing
	if c.len() != 2 || c.bytes != 8 {
		t.Fatalf("refresh: len %d bytes %d", c.len(), c.bytes)
	}
	if _, ok := c.get(cacheKey{"other", "d"}); ok {
		t.Fatal("keys are per app")
	}
}
