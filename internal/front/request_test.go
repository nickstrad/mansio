package front

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

var lim = Limits{MaxBody: 4096, MaxSQL: 200, MaxArgs: 3, MaxRows: 10}.WithDefaults()

func TestDecodeRequest(t *testing.T) {
	sql := func(n int) string { return "SELECT " + strings.Repeat("1", n-7) } // n bytes
	tests := []struct {
		name string
		body string
		ok   bool
		msg  string // part of the client message on failure
	}{
		{"read", `{"db":"d1","kind":"read","sql":"SELECT 1"}`, true, ""},
		{"write with id", `{"db":"d1","request_id":"r-1","kind":"write","sql":"INSERT INTO t VALUES (?)","args":[1]}`, true, ""},
		{"write without request_id", `{"db":"d1","kind":"write","sql":"INSERT INTO t VALUES (1)"}`, false, "request_id"},
		{"read with bad request_id", `{"db":"d1","request_id":"a b","kind":"read","sql":"SELECT 1"}`, false, "request_id"},
		{"request_id at max (on)", `{"db":"d1","request_id":"` + strings.Repeat("r", MaxRequestID) + `","kind":"read","sql":"SELECT 1"}`, true, ""},
		{"request_id over max (off)", `{"db":"d1","request_id":"` + strings.Repeat("r", MaxRequestID+1) + `","kind":"read","sql":"SELECT 1"}`, false, "request_id"},
		{"empty body", ``, false, "not a valid"},
		{"not an object", `[1]`, false, "not a valid"},
		{"unknown field", `{"db":"d1","kind":"read","sql":"SELECT 1","x":1}`, false, "unknown field"},
		{"trailing data", `{"db":"d1","kind":"read","sql":"SELECT 1"} {}`, false, "after the request"},
		{"trailing space ok", `{"db":"d1","kind":"read","sql":"SELECT 1"}  ` + "\n", true, ""},
		{"op reserved", `{"db":"d1","kind":"read","op":{"get":1}}`, false, "op payloads"},
		{"missing db", `{"kind":"read","sql":"SELECT 1"}`, false, "db must"},
		{"db with slash", `{"db":"a/b","kind":"read","sql":"SELECT 1"}`, false, "db may hold"},
		{"db dot-dot", `{"db":"..","kind":"read","sql":"SELECT 1"}`, false, "must not start"},
		{"db at max (on)", `{"db":"` + strings.Repeat("d", 128) + `","kind":"read","sql":"SELECT 1"}`, true, ""},
		{"db over max (off)", `{"db":"` + strings.Repeat("d", 129) + `","kind":"read","sql":"SELECT 1"}`, false, "db must"},
		{"bad kind", `{"db":"d1","kind":"READ","sql":"SELECT 1"}`, false, "kind"},
		{"sql at max (on)", `{"db":"d1","kind":"read","sql":"` + sql(200) + `"}`, true, ""},
		{"sql over max (off)", `{"db":"d1","kind":"read","sql":"` + sql(201) + `"}`, false, "sql over"},
		{"args at max (on)", `{"db":"d1","kind":"read","sql":"SELECT ?, ?, ?","args":[1,2,3]}`, true, ""},
		{"args over max (off)", `{"db":"d1","kind":"read","sql":"SELECT ?","args":[1,2,3,4]}`, false, "more than 3"},
		{"array arg", `{"db":"d1","kind":"read","sql":"SELECT ?","args":[[1]]}`, false, "args[0]"},
		{"object arg", `{"db":"d1","kind":"read","sql":"SELECT ?","args":[{"x":1}]}`, false, "args[0]"},
		{"huge int arg", `{"db":"d1","kind":"read","sql":"SELECT ?","args":[99999999999999999999]}`, false, "64 bits"},
		{"huge float arg", `{"db":"d1","kind":"read","sql":"SELECT ?","args":[1e999]}`, false, "range"},
		{"bad b64", `{"db":"d1","kind":"read","sql":"SELECT ?","args":[{"b64":"***"}]}`, false, "base64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(tc.body), lim)
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok=%v", err, tc.ok)
			}
			var bad *BadRequestError
			if err != nil && (!errors.As(err, &bad) || !strings.Contains(bad.Msg, tc.msg)) {
				t.Fatalf("err %v, want a BadRequestError mentioning %q", err, tc.msg)
			}
		})
	}
	big := bytes.Repeat([]byte(" "), lim.MaxBody+1)
	var bad *BadRequestError
	if _, err := DecodeRequest(big, lim); !errors.As(err, &bad) || bad.Status != 413 {
		t.Fatalf("body over max: %v", err)
	}
	if _, err := DecodeRequest(big[:lim.MaxBody], lim); errors.As(err, &bad) && bad.Status == 413 {
		t.Fatal("body at max refused as too large")
	}
}

func TestDecodeArgs(t *testing.T) {
	r, err := DecodeRequest([]byte(`{"db":"d","kind":"read","sql":"SELECT ?","args":
		[null, true, false, 0, -9223372036854775808, 9223372036854775807, 1.5, 1e3, "s", {"b64":"AAH/"}, {"b64":"AAH_"}]}`),
		Limits{MaxArgs: 20}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	want := []any{nil, int64(1), int64(0), int64(0), int64(math.MinInt64), int64(math.MaxInt64), 1.5, 1000.0, "s",
		[]byte{0, 1, 255}, []byte{0, 1, 255}}
	for i := range want {
		if a, b := r.Args[i], want[i]; !equalValue(a, b) {
			t.Errorf("arg %d: %#v, want %#v", i, a, b)
		}
	}
	if _, err := decodeArg(nil); err == nil {
		t.Error("empty raw value accepted")
	}
}

func equalValue(a, b any) bool {
	if x, ok := a.([]byte); ok {
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return a == b
}

func TestCheckSQL(t *testing.T) {
	tests := []struct {
		kind Kind
		sql  string
		ok   bool
	}{
		{KindRead, "SELECT 1", true},
		{KindRead, "  select 1;  ", true},
		{KindRead, "WITH a AS (SELECT 1) SELECT * FROM a", true},
		{KindRead, "VALUES (1)", true},
		{KindRead, "SELECT 1; SELECT 2", false},
		{KindRead, "SELECT 1;;", false},
		{KindRead, "INSERT INTO t VALUES (1)", false},
		{KindRead, "WITH a AS (SELECT 1) DELETE FROM t", false},
		{KindRead, "PRAGMA table_info(t)", false},
		{KindRead, "EXPLAIN SELECT 1", false},
		{KindRead, "/* c */ SELECT 1", false},
		{KindRead, "", false},
		{KindRead, " ; ", false},
		{KindRead, "SELECT '\x00'", false},
		{KindRead, "SELECT '\xff'", false},
		{KindWrite, "INSERT INTO t VALUES (?)", true},
		{KindWrite, "update t set a = 1", true},
		{KindWrite, "SELECT 1", true},
		{KindWrite, "CREATE TABLE x (a)", true},
		{KindWrite, "BEGIN", false},
		{KindWrite, "COMMIT", false},
		{KindWrite, "SAVEPOINT a", false},
		{KindWrite, "ATTACH 'x' AS y", false},
		{KindWrite, "PRAGMA journal_mode = DELETE", false},
		{KindWrite, "VACUUM", false},
		{KindWrite, "DELETE FROM t; DROP TABLE t", false},
	}
	for _, tc := range tests {
		if err := checkSQL(tc.kind, tc.sql); (err == nil) != tc.ok {
			t.Errorf("%s %q: err %v, want ok=%v", tc.kind, tc.sql, err, tc.ok)
		}
	}
}

func TestWireValuesEncodeBlobs(t *testing.T) {
	rows := wireValues([][]any{{int64(1), []byte{0, 255}, "s", nil}})
	b, _ := json.Marshal(rows)
	if string(b) != `[[1,{"b64":"AP8="},"s",null]]` {
		t.Fatalf("%s", b)
	}
	if KindRead.Perm() == KindWrite.Perm() {
		t.Fatal("read and write need the same permission")
	}
}

// FuzzDecodeRequest: arbitrary bytes never panic; an accepted request satisfies the Request invariant, and its
// re-encoding decodes to the same request.
func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"db":"d1","kind":"read","sql":"SELECT ?","args":[1,"a",null,true,1.5,{"b64":"AA=="}]}`))
	f.Add([]byte(`{"db":"d1","request_id":"r","kind":"write","sql":"INSERT INTO t VALUES (?)","args":[1]}`))
	f.Add([]byte(`{"db":"d1","kind":"read","op":{}}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, body []byte) {
		r, err := DecodeRequest(body, lim)
		if err != nil {
			var bad *BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("non-validation error %T %v", err, err)
			}
			return
		}
		if checkID("db", r.DB, 128) != nil || (r.Kind != KindRead && r.Kind != KindWrite) || checkSQL(r.Kind, r.SQL) != nil ||
			len(r.Args) > lim.MaxArgs || (r.Kind == KindWrite && r.RequestID == "") {
			t.Fatalf("accepted an invalid request %+v", r)
		}
		args := make([]any, len(r.Args))
		for i, a := range r.Args {
			switch v := a.(type) {
			case nil, int64, string:
				args[i] = v
			case float64:
				if math.IsInf(v, 0) || math.IsNaN(v) {
					t.Fatalf("non-finite float accepted")
				}
				args[i] = v
			case []byte:
				args[i] = Blob{B64: base64Std(v)}
			default:
				t.Fatalf("arg %d has type %T", i, a)
			}
		}
		again, err := json.Marshal(map[string]any{"db": r.DB, "request_id": r.RequestID, "kind": r.Kind, "sql": r.SQL, "args": args})
		if err != nil {
			t.Fatal(err)
		}
		r2, err := DecodeRequest(again, lim)
		if err != nil {
			t.Fatalf("re-encoded request refused: %v\n%s", err, again)
		}
		if r2.DB != r.DB || r2.SQL != r.SQL || r2.Kind != r.Kind || len(r2.Args) != len(r.Args) {
			t.Fatalf("round trip changed the request: %+v vs %+v", r, r2)
		}
		for i := range r.Args {
			x, y := r.Args[i], r2.Args[i]
			if fx, ok := x.(float64); ok { // 1e3 re-encodes as 1000 and decodes as int64
				if fy, ok := y.(float64); ok {
					y = fy
				} else if iy, ok := y.(int64); ok {
					y = float64(iy)
				}
				x = fx
			}
			if !equalValue(x, y) {
				t.Fatalf("arg %d: %#v → %#v", i, x, y)
			}
		}
	})
}

func base64Std(b []byte) string {
	out, _ := json.Marshal(b) // encoding/json uses std base64 for []byte
	return strings.Trim(string(out), `"`)
}
