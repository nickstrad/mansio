package ir_test

import (
	"testing"
	"unicode/utf8"

	"github.com/nickstrad/mansio/server/internal/ir"
)

func addDecodeSeeds(f *testing.F) {
	for _, v := range loadVectors(f, "decode.json") {
		if raw, ok := v.Value("raw").(string); ok && len(raw) <= ir.MaxBodyBytes {
			f.Add([]byte(raw))
		}
	}
}

// FuzzDecodeRequest: DecodeBody never panics; it enforces the size cap and the op limit; an accepted request
// re-decodes from its canonical encoding to the same request; and it rejects an unknown member added to
// the envelope or to an operation.
func FuzzDecodeRequest(f *testing.F) {
	addDecodeSeeds(f)
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := ir.DecodeBody(body)
		if len(body) > ir.MaxBodyBytes {
			if err == nil || err.Tag != ir.TagTooLarge {
				t.Fatalf("%d bytes: %v", len(body), err)
			}
			return
		}
		if err != nil {
			if err.Tag != ir.TagSchema || err.Status() != 400 {
				t.Fatalf("decode failure is not a SchemaError: %v", err)
			}
			return
		}
		if n := len(req.Ops); n < 1 || n > ir.MaxOps {
			t.Fatalf("%d ops accepted", n)
		}
		canonical := ir.Canonical(req.JSON())
		again, err := ir.DecodeBody([]byte(canonical))
		if err != nil || ir.Canonical(again.JSON()) != canonical {
			t.Fatalf("canonical %s does not re-decode: %v", canonical, err)
		}
		parsed, _ := ir.Parse(string(body))
		top := parsed.(*ir.Object)
		top.Set("zz_unknown", true)
		if _, err := ir.DecodeValue(top); err == nil || err.Reason() != ir.ReasonUnknownField || err.Path() != "zz_unknown" {
			t.Fatalf("unknown envelope member: %v", err)
		}
		top.Set("zz_unknown", ir.Undefined{})
		op := top.Value("ops").([]any)[0].(*ir.Object)
		op.Set("zz_unknown", true)
		if _, err := ir.DecodeValue(top); err == nil || err.Reason() != ir.ReasonUnknownField || err.Path() != "ops[0].zz_unknown" || !err.HasOp || err.Op != 0 {
			t.Fatalf("unknown op member: %v", err)
		}
	})
}

// FuzzCanonicalJSON: for any text Parse accepts, Canonical and Stringify both round-trip to an equal
// value, and Canonical is a fixed point.
func FuzzCanonicalJSON(f *testing.F) {
	addDecodeSeeds(f)
	for _, s := range []string{`{"b":[1,-0,1e21,1e-7,"x"],"a":null,"1":true}`, `"` + esc("00e9") + esc("d83d") + esc("de00") + `"`, `[[[]]]`, `{}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if !utf8.Valid(data) {
			return
		}
		v, err := ir.Parse(string(data))
		if err != nil {
			return
		}
		c := ir.Canonical(v)
		w, err := ir.Parse(c)
		if err != nil {
			t.Fatalf("canonical %q does not parse: %v", c, err)
		}
		if !ir.Equal(v, w) || ir.Canonical(w) != c {
			t.Fatalf("canonical round trip changed %q -> %q", data, c)
		}
		s := ir.Stringify(v)
		if u, err := ir.Parse(s); err != nil || !ir.Equal(v, u) {
			t.Fatalf("stringify round trip %q -> %q: %v", data, s, err)
		}
	})
}
