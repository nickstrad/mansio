package docschema_test

import (
	"math"
	"strings"
	"testing"
	"testing/quick"

	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

func node(t *testing.T, text string) *docschema.Node {
	t.Helper()
	v, err := ir.Parse(`{"t":{"fields":{"f":{"node":` + text + `}},"storage":"columns","indexes":[]}}`)
	if err != nil {
		t.Fatal(err)
	}
	s, derr := docschema.Decode(v)
	if derr != nil {
		t.Fatal(derr)
	}
	table, _ := s.Table("t")
	n, _ := table.Field("f")
	return n
}

// TestValidateOnOffPoints pins each boundary of core/schema.ts validate and the storage rule.
func TestValidateOnOffPoints(t *testing.T) {
	integer := node(t, `{"kind":"integer"}`)
	enum := node(t, `{"kind":"enum","values":["a","b"]}`)
	fixed := node(t, `{"kind":"array","item":{"kind":"integer"},"length":3}`)
	nullable := node(t, `{"kind":"string","nullable":true}`)
	for _, c := range []struct {
		n     *docschema.Node
		value any
		ok    bool
	}{
		{integer, float64(ir.MaxSafeInteger), true}, {integer, float64(ir.MaxSafeInteger) + 1, false},
		{integer, -float64(ir.MaxSafeInteger), true}, {integer, -float64(ir.MaxSafeInteger) - 1, false},
		{integer, 1.5, false}, {integer, "1", false}, {integer, nil, false}, {integer, math.Copysign(0, -1), true},
		{enum, "a", true}, {enum, "A", false}, {enum, "c", false}, {enum, nil, false},
		{fixed, []any{1.0, 2.0, 3.0}, true}, {fixed, []any{1.0, 2.0}, false}, {fixed, []any{1.0, 2.0, 3.0, 4.0}, false},
		{fixed, []any{1.0, 2.5, 3.0}, false}, {nullable, nil, true}, {nullable, ir.Undefined{}, false},
	} {
		_, err := docschema.Validate(c.n, c.value, "f")
		if (err == nil) != c.ok {
			t.Errorf("Validate(%s, %s) = %v, want ok=%v", c.n.Kind, ir.Stringify(c.value), err, c.ok)
		}
	}
	_, err := docschema.Validate(fixed, []any{1.0, "x", 3.0}, "grid")
	if err == nil || err.Path() != "grid[1]" || err.Field("expected") != "integer" {
		t.Errorf("element path: %v", err)
	}
}

// TestIntegerSafetyProperty: an integer node accepts exactly the safe integers.
func TestIntegerSafetyProperty(t *testing.T) {
	integer := node(t, `{"kind":"integer"}`)
	prop := func(f float64, scale uint8) bool {
		v := math.Trunc(f) * math.Pow(2, float64(scale%60))
		_, err := docschema.Validate(integer, v, "f")
		safe := v == math.Trunc(v) && math.Abs(v) <= ir.MaxSafeInteger
		return (err == nil) == safe
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 5000}); err != nil {
		t.Fatal(err)
	}
}

// TestStorableProperty: a body is storable exactly when none of its strings, at any depth, holds U+0000,
// and the error names the first such string's path.
func TestStorableProperty(t *testing.T) {
	prop := func(a, b string, nul uint8) bool {
		a, b = strings.ReplaceAll(a, "\x00", ""), strings.ReplaceAll(b, "\x00", "")
		path := ""
		switch nul % 3 {
		case 1:
			a, path = a+"\x00", "x"
		case 2:
			b, path = "\x00"+b, "y[1]"
		}
		err := docschema.Storable(ir.NewObject("x", a, "y", []any{1.0, b}), "")
		if path == "" {
			return err == nil
		}
		return err != nil && err.Path() == path && err.Field("expected") == "text without U+0000"
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// TestProtoName: __proto__ is an ordinary name (Deno disables the accessor, so core/ir/schema.ts stores it as
// an own property), and so reserved like any other _-prefixed name.
func TestProtoName(t *testing.T) {
	for text, want := range map[string]string{
		`{"t":{"fields":{"__proto__":{"node":{"kind":"string"}}},"storage":"json","indexes":[]}}`: "reserved-field",
		`{"__proto__":{"fields":{},"storage":"json","indexes":[]}}`:                               "reserved-table",
	} {
		v, _ := ir.Parse(text)
		if _, err := docschema.Decode(v); err == nil || err.Reason() != want || err.Path() != "__proto__" {
			t.Errorf("%s: got %v, want %s", text, err, want)
		}
	}
}

// TestDecodeOrder pins JavaScript's iteration order in the declaration checks: an array-index name is
// visited first, whatever its position in the text.
func TestDecodeOrder(t *testing.T) {
	v, _ := ir.Parse(`{"t":{"fields":{"_x":{"node":{"kind":"string"}},"1":{"node":{"kind":"string"}}},"storage":"json","indexes":[]}}`)
	if _, err := docschema.Decode(v); err == nil || err.Reason() != "invalid-identifier" || err.Path() != "1" {
		t.Errorf("got %v, want invalid-identifier at 1", err)
	}
}
