package ir_test

import (
	"math"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/ir"
)

// TestFormatNumber pins ECMAScript Number::toString at its notation boundaries (not Go's FormatFloat ones).
// esc is a JSON \\u escape, spelled out so no tool turns it into the character.
func esc(hex string) string { return `\` + "u" + hex }

func TestFormatNumber(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{0, "0"}, {math.Copysign(0, -1), "0"}, {1, "1"}, {-1.5, "-1.5"}, {0.1, "0.1"}, {123.456, "123.456"},
		{1e-6, "0.000001"}, {1e-7, "1e-7"}, {1.5e-7, "1.5e-7"}, {123e-9, "1.23e-7"},
		{1e20, "100000000000000000000"}, {1e21, "1e+21"}, {1.5e21, "1.5e+21"}, {123456789012345680000, "123456789012345680000"},
		{9007199254740993, "9007199254740992"}, {5e-324, "5e-324"}, {math.MaxFloat64, "1.7976931348623157e+308"},
		{math.NaN(), "NaN"}, {math.Inf(1), "Infinity"}, {math.Inf(-1), "-Infinity"},
	} {
		if got := ir.FormatNumber(c.f); got != c.want {
			t.Errorf("FormatNumber(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestParseNumbers(t *testing.T) {
	for _, c := range []struct {
		text string
		want float64
	}{
		{"10.0", 10}, {"1e0", 1}, {"-0", math.Copysign(0, -1)}, {"9007199254740993", 9007199254740992}, {"1e-400", 0},
		{"1E+2", 100}, {"0.5e-1", 0.05},
	} {
		v, err := ir.Parse(c.text)
		if err != nil || v.(float64) != c.want || math.Signbit(v.(float64)) != math.Signbit(c.want) {
			t.Errorf("Parse(%s) = %v, %v; want %v", c.text, v, err, c.want)
		}
	}
	for _, text := range []string{"1e400", "-1e400", "01", "1.", ".5", "-", "+1", "1e", "0x10", "NaN", "Infinity"} {
		if v, err := ir.Parse(text); err == nil {
			t.Errorf("Parse(%s) = %v, want an error", text, v)
		}
	}
}

func TestParseRefusals(t *testing.T) {
	for _, c := range []struct{ text, path, expected string }{
		{`{"a":1,"a":2}`, "a", "unique member name"},
		{`{"a":1,"` + esc("0061") + `":2}`, "a", "unique member name"},
		{`{"a":[1,2,{"b":"\ud800"}]}`, "a[2].b", "well-formed Unicode text"},
		{`{"a":"\udc00\ud800"}`, "a", "well-formed Unicode text"},
		{`{"a":1e400}`, "a", "finite number"},
		{"\xef\xbb\xbf{}", "body", "JSON"},
		{`{"a":1}x`, "body", "JSON"},
		{`{"a":tru}`, "a", "JSON"},
		{`[1,]`, "[1]", "JSON"},
		{`{"a":"x` + "\n" + `"}`, "a", "JSON"},
		{`{"a":"\x"}`, "a", "JSON"},
		{`{"a":"\u12"}`, "a", "JSON"},
		{strings.Repeat("[", 65) + strings.Repeat("]", 65), strings.Repeat("[0]", 64), "nesting at most 64 arrays/objects deep"},
	} {
		_, err := ir.Parse(c.text)
		if err == nil || err.Path() != c.path || err.Field("expected") != c.expected || err.Reason() != "type-mismatch" {
			t.Errorf("Parse(%q) = %v, want path %q expected %q", c.text, err, c.path, c.expected)
		}
	}
	if _, err := ir.Parse(strings.Repeat("[", 64) + strings.Repeat("]", 64)); err != nil {
		t.Errorf("64 containers: %v", err)
	}
	v, err := ir.Parse(`"` + esc("d83d") + esc("de00") + esc("00e9") + `\n"`)
	if err != nil || v != string(rune(0x1F600))+string(rune(0xE9))+"\n" {
		t.Errorf("surrogate pair: %q %v", v, err)
	}
	// A lone surrogate in a member name is reported at that member, and JSON-escaped back as TS writes it.
	_, err = ir.Parse(`{"x":{"\ud800":1}}`)
	if err == nil || ir.Stringify(err.Field("path")) != `"x.\ud800"` {
		t.Errorf("lone surrogate name: %v", err)
	}
}

// TestObjectOrder pins ECMAScript own-property order: array indices first, ascending, then insertion order.
func TestObjectOrder(t *testing.T) {
	v, err := ir.Parse(`{"b":1,"2":2,"a":3,"10":4,"1":5,"01":6,"4294967295":7,"4294967294":8}`)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(v.(*ir.Object).Keys(), ",")
	if want := "1,2,10,4294967294,b,a,01,4294967295"; got != want {
		t.Errorf("keys %s, want %s", got, want)
	}
	o := ir.NewObject("a", 1.0, "b", 2.0)
	o.Set("a", 3.0)
	o.Set("b", ir.Undefined)
	if ir.Stringify(o) != `{"a":3}` {
		t.Errorf("set/remove: %s", ir.Stringify(o))
	}
}

func TestStringifyAndCanonical(t *testing.T) {
	pua, emoji, ls := string(rune(0xE000)), string(rune(0x1F600)), string(rune(0x2028))
	v, err := ir.Parse(`{"z":[1,null,true,"a\"\\` + esc("0001") + esc("001f") + esc("007f") + esc("2028") + `/"],"` +
		esc("e000") + `":1,"` + emoji + `":2,"a":{"y":-0,"x":1e21}}`)
	if err != nil {
		t.Fatal(err)
	}
	text := `"a\"\\` + esc("0001") + esc("001f") + "\x7f" + ls + `/"`
	if got, want := ir.Stringify(v), `{"z":[1,null,true,`+text+`],"`+pua+`":1,"`+emoji+`":2,"a":{"y":0,"x":1e+21}}`; got != want {
		t.Errorf("Stringify\n got %s\nwant %s", got, want)
	}
	// UTF-16 order: U+1F600 is the pair D83D DE00, which sorts before U+E000 (byte order says the opposite).
	if got, want := ir.Canonical(v), `{"a":{"x":1e+21,"y":0},"z":[1,null,true,`+text+`],"`+emoji+`":2,"`+pua+`":1}`; got != want {
		t.Errorf("Canonical\n got %s\nwant %s", got, want)
	}
	if got := ir.StringifyIndent(ir.NewObject("a", []any{1.0, ir.NewObject()}, "b", []any{}), "  "); got != "{\n  \"a\": [\n    1,\n    {}\n  ],\n  \"b\": []\n}" {
		t.Errorf("StringifyIndent: %q", got)
	}
}

func TestToString(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{nil, "null"}, {true, "true"}, {1.5, "1.5"}, {"x", "x"}, {[]any{1.0, nil, []any{"a", 2.0}}, "1,,a,2"},
		{ir.NewObject(), "[object Object]"}, {ir.Undefined, "undefined"},
	} {
		if got := ir.ToString(c.v); got != c.want {
			t.Errorf("ToString(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestErrorWire(t *testing.T) {
	err := ir.SchemaErr("type-mismatch", "ops[1].id", "string", 5.0).WithOp(1)
	if got, want := ir.Stringify(err.Wire()), `{"error":{"tag":"SchemaError","message":"SchemaError: reason=\"type-mismatch\" path=\"ops[1].id\" expected=\"string\" actual=5","op":1,"fields":{"reason":"type-mismatch","path":"ops[1].id","expected":"string","actual":5}}}`; got != want {
		t.Errorf("wire\n got %s\nwant %s", got, want)
	}
	if got := ir.Stringify(ir.NewError(ir.TagStorage, "detail", "disk").Wire()); got != `{"error":{"tag":"StorageError","message":"StorageError"}}` {
		t.Errorf("5xx wire: %s", got)
	}
	for tag, status := range map[string]int{ir.TagSchema: 400, ir.TagCompile: 400, ir.TagNotFound: 404, ir.TagConflict: 409,
		ir.TagSchemaDrift: 409, ir.TagTooLarge: 413, ir.TagDecode: 500, ir.TagStorage: 500, ir.TagDefect: 500, ir.TagStmtDeadline: 504} {
		if got := ir.NewError(tag).Status(); got != status {
			t.Errorf("%s: status %d, want %d", tag, got, status)
		}
	}
}

func TestResolve(t *testing.T) {
	v, _ := ir.Parse(`{"a":{"$ref":1},"b":[{"$ref":0},"x"],"c":{"d":{"$ref":1}}}`)
	got := ir.Resolve(v, []string{"i0", "i1"})
	if ir.Stringify(got) != `{"a":"i1","b":["i0","x"],"c":{"d":"i1"}}` {
		t.Errorf("Resolve: %s", ir.Stringify(got))
	}
	if ir.ResolveID(ir.IDRef{Ref: 1, IsRef: true}, []string{"i0", "i1"}) != "i1" || ir.ResolveID(ir.IDRef{ID: "z"}, nil) != "z" {
		t.Error("ResolveID")
	}
}
