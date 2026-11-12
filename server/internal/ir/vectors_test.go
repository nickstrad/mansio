package ir_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/ir"
)

// vectorDir is the shared corpus at the repo root (ir/vectors/README.md).
var vectorDir = filepath.Join("..", "..", "..", "ir", "vectors")

func loadVectors(t testing.TB, name string) []*ir.Object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatal(err)
	}
	v, perr := ir.Parse(string(data))
	if perr != nil {
		t.Fatalf("%s: %v", name, perr)
	}
	var out []*ir.Object
	for _, item := range v.(*ir.Object).Value("vectors").([]any) {
		out = append(out, item.(*ir.Object))
	}
	return out
}

// assertWireError is the README's comparison: tag, status and op exactly; fields as a subset.
func assertWireError(t *testing.T, name string, got *ir.Error, want *ir.Object) {
	t.Helper()
	wire := ir.Stringify(got.Wire())
	if got.Tag != want.Value("tag") {
		t.Errorf("%s: tag %s, want %v", name, wire, want.Value("tag"))
	}
	if float64(got.Status()) != want.Value("status") {
		t.Errorf("%s: status %d, want %v (%s)", name, got.Status(), want.Value("status"), wire)
	}
	wantOp, hasOp := want.Get("op")
	if hasOp != got.HasOp || (hasOp && wantOp != float64(got.Op)) {
		t.Errorf("%s: op %v/%v, want %v (%s)", name, got.HasOp, got.Op, wantOp, wire)
	}
	if fields, ok := want.Value("fields").(*ir.Object); ok {
		for _, k := range fields.Keys() {
			if !ir.Equal(got.Field(k), fields.Value(k)) {
				t.Errorf("%s: fields.%s = %s, want %s (%s)", name, k, ir.Stringify(got.Field(k)), ir.Stringify(fields.Value(k)), wire)
			}
		}
	}
}

func vectorBytes(t *testing.T, v *ir.Object) []byte {
	var body []byte
	if b64, ok := v.Value("base64").(string); ok {
		var err error
		if body, err = base64.StdEncoding.DecodeString(b64); err != nil {
			t.Fatal(err)
		}
	} else {
		body = []byte(v.Value("raw").(string))
	}
	if pad, ok := v.Value("pad").(float64); ok {
		body = append(body, []byte(strings.Repeat(" ", int(pad)-len(body)))...)
	}
	return body
}

func TestDecodeVectors(t *testing.T) {
	vectors := loadVectors(t, "decode.json")
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vectors {
		name := v.Value("name").(string)
		expect := v.Value("expect").(*ir.Object)
		req, err := ir.DecodeBody(vectorBytes(t, v))
		if expect.Value("ok") == true {
			if err != nil {
				t.Errorf("%s: unexpected %v", name, err)
				continue
			}
			if got := ir.Canonical(req.JSON()); got != expect.Value("canonical") {
				t.Errorf("%s: canonical\n got %s\nwant %s", name, got, expect.Value("canonical"))
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected an error, decoded %s", name, ir.Canonical(req.JSON()))
			continue
		}
		assertWireError(t, name, err, expect.Value("error").(*ir.Object))
	}
}
