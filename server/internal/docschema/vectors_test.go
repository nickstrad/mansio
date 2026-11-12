package docschema_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/mansio/server/internal/doccompile"
	"github.com/nickstrad/mansio/server/internal/docschema"
	"github.com/nickstrad/mansio/server/internal/ir"
)

var vectorDir = filepath.Join("..", "..", "..", "intermediate-representation", "vectors")

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
	if len(out) == 0 {
		t.Fatalf("%s: no vectors", name)
	}
	return out
}

// checkError is the README's wire comparison: tag and status exactly, no op, fields as a subset.
func checkError(t *testing.T, name string, got *ir.Error, want *ir.Object) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: ok, want %s", name, ir.Stringify(want))
		return
	}
	if got.Tag != want.Value("tag") || float64(got.Status()) != want.Value("status") || got.HasOp {
		t.Errorf("%s: got %s, want %s", name, ir.Stringify(got.Wire()), ir.Stringify(want))
	}
	if fields, ok := want.Value("fields").(*ir.Object); ok {
		for _, k := range fields.Keys() {
			if !ir.Equal(got.Field(k), fields.Value(k)) {
				t.Errorf("%s: fields.%s = %s, want %s", name, k, ir.Stringify(got.Field(k)), ir.Stringify(fields.Value(k)))
			}
		}
	}
}

func TestValidationVectors(t *testing.T) {
	for _, v := range loadVectors(t, "validation.json") {
		name := v.Value("name").(string)
		s, err := docschema.Decode(ir.NewObject("t", ir.NewObject(
			"fields", v.Value("fields"), "storage", "columns", "indexes", []any{})))
		if err != nil {
			t.Fatalf("%s: schema: %v", name, err)
		}
		table, _ := s.Table("t")
		got, verr := docschema.Body(table, v.Value("value"), v.Value("mode") == "patch")
		expect := v.Value("expect").(*ir.Object)
		if want, ok := expect.Get("error"); ok {
			checkError(t, name, verr, want.(*ir.Object))
			continue
		}
		if verr != nil {
			t.Errorf("%s: unexpected %v", name, verr)
		} else if ir.Stringify(got) != ir.Stringify(expect.Value("value")) { // member order and -0 too
			t.Errorf("%s: got %s, want %s", name, ir.Stringify(got), ir.Stringify(expect.Value("value")))
		}
	}
}

func TestSchemaVectors(t *testing.T) {
	for _, v := range loadVectors(t, "schema.json") {
		name := v.Value("name").(string)
		s, err := docschema.Decode(v.Value("schema"))
		if err == nil {
			_, err = doccompile.DDL(s)
		}
		expect := v.Value("expect").(*ir.Object)
		if want, ok := expect.Get("error"); ok {
			checkError(t, name, err, want.(*ir.Object))
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected %v", name, err)
			continue
		}
		text := s.FingerprintText()
		if text != expect.Value("text") {
			t.Errorf("%s: text\n got %s\nwant %s", name, text, expect.Value("text"))
		}
		if fp := docschema.Fingerprint(text); fp != expect.Value("fingerprint") {
			t.Errorf("%s: fingerprint %s, want %s", name, fp, expect.Value("fingerprint"))
		}
	}
}
