package eventlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The goldens are the request log's bytes for these records, by name, in testdata/goldens.jsonl: the record
// format's definition by example (docs/observability.md documents it). They were first written by the
// TypeScript encoder this package had to match, which is gone (ADR 0014). A difference is a format change:
// update the file only on purpose, as the log's readers (collectors, queries) depend on the bytes.
var goldens = []struct {
	name string
	ev   Event
}{
	{"http with redacted detail", Event{Level: Warn, TraceID: "t-1", AppID: "tic-tac-toe", Payload: HTTP{
		Method: "POST", Path: "/v1/ops", HandlerKind: "mutation", Name: "makeMove", Status: 409, Duration: 1500 * time.Microsecond,
		BytesIn: 120, BytesOut: 77, ErrorTag: "ConflictError", ErrorDetail: map[string]any{
			"tag": "ConflictError", "message": "line1\nline2 Bearer abc.def",
			"fields": map[string]any{"Authorization": "Bearer x", "x-api-token": "s", "nested": []any{1, "Bearer zz", nil, true}},
		}}}},
	{"handler", Event{TraceID: "t-1", SpanID: "s1", ParentSpanID: "p1", AppID: "a", Payload: Handler{
		HandlerKind: "query", Name: "getGame", Outcome: "ok", Duration: 250 * time.Microsecond}}},
	{"sql never carries bind values", Event{TraceID: "t-1", SpanID: "s1", AppID: "a", Payload: SQL{
		Engine: "sqlite", Dialect: "sqlite", StatementKind: "select", TableName: "games", SQLText: `SELECT * FROM "games" WHERE "_id" = ?`,
		BindCount: 1, Duration: 12 * time.Microsecond, RowCount: 1, Outcome: "ok"}}},
	{"tx", Event{TraceID: "t-1", SpanID: "s1", AppID: "a", Payload: Tx{Engine: "sqlite", Outcome: "rollback", Operations: 3,
		Duration: 2 * time.Millisecond, QueueWait: 500 * time.Microsecond, ErrorTag: "StorageError", ErrorDetail: map[string]any{"reason": "x"}}}},
	{"queue drop report", Event{Level: Warn, TraceID: SinkTraceID, Payload: Queue{Queue: "events", Depth: 10, Cap: 10, Event: "dropped", Dropped: 3}}},
	{"lifecycle with S3 secrets", Event{TraceID: "t-1", Payload: Lifecycle{Phase: "startup", Detail: map[string]any{
		"url": "https://s3/x?X-Amz-Signature=abc&X-Amz-Credential=def&ok=1", "AWS_SECRET_ACCESS_KEY": "k", "sessionToken": "t",
		"list": []any{"a b", map[string]any{"Cookie": "c"}}}}}},
	{"backup with absent optionals", Event{TraceID: "t-1", AppID: "a", Payload: Backup{Phase: "publish-latest", Emitter: "backups", RunID: "r1",
		Bytes: -1, Attempts: -1, Duration: 3 * time.Millisecond, Outcome: "ok"}}},
	{"lifecycle with JS whitespace, case folding, key order and numbers", Event{TraceID: "t-1", Payload: Lifecycle{Phase: "js-semantics", Detail: map[string]any{
		"lineSeparator": "Bearer\u2028SECRET1", "nbsp": "Bearer\u00a0SECRET2", "verticalTab": "Bearer\vSECRET3", "bom": "Bearer\ufeffSECRET4",
		"ideographic": "Bearer\u3000SECRET5", "amzNbsp": "u?X-Amz-Signature=x\u00a0kept", "longS": "X-Amz-\u017fignature=kept",
		"COO\u212aIE": "kelvin sign lower-cases to k", "\uff61": "sorts after", "\U00010000": "sorts before (UTF-16)",
		"big": 1e21, "small": 1e-7, "plain": 123.456,
	}}}},
}

var goldenStamp = stamp{EventID: "e1", TS: time.Date(2026, 11, 6, 10, 11, 12, 345e6, time.UTC), Role: "server", PID: 42}

// TestLineMatchesTS: every kind serializes byte for byte as testdata/goldens.jsonl says (the encoding the
// TypeScript encoder had), redaction, JS whitespace and case folding, key order and number format included.
func TestLineMatchesTS(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "goldens.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var g struct{ Name, Line string }
		if err := json.Unmarshal([]byte(l), &g); err != nil {
			t.Fatal(err)
		}
		want[g.Name] = g.Line
	}
	if len(want) != len(goldens) {
		t.Fatalf("%d goldens in the file, %d here: keep record_test.go and testdata/goldens.jsonl in step", len(want), len(goldens))
	}
	for _, g := range goldens {
		t.Run(g.name, func(t *testing.T) {
			w, ok := want[g.name]
			if !ok {
				t.Fatalf("no golden named %q", g.name)
			}
			if got := string(goldenStamp.line(g.ev)); got != w {
				t.Fatalf("line differs from testdata/goldens.jsonl\ngot:  %s\nwant: %s", got, w)
			}
		})
	}
}

func TestLineIsOneValidJSONObject(t *testing.T) {
	ev := Event{TraceID: "t\nx", Payload: SQL{SQLText: "SELECT 1\r\n-- Bearer secret ", RowCount: -1,
		ErrorDetail: map[string]any{"cause": struct{ Token, Msg string }{"tok", "Bearer q"}, "deep": nest(40)}}}
	line := string(goldenStamp.line(ev))
	if strings.ContainsAny(line, "\n\r  ") {
		t.Fatalf("line breaks in %q", line)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, line)
	}
	for _, leak := range []string{"secret", "tok", "Bearer q"} {
		if strings.Contains(line, leak) {
			t.Errorf("%q leaked: %s", leak, line)
		}
	}
	if !strings.Contains(line, `"<max-depth>"`) {
		t.Errorf("deep nesting not cut: %s", line)
	}
	if got["rowCount"] != nil || got["traceId"] != "t x" {
		t.Errorf("rowCount %v traceId %q", got["rowCount"], got["traceId"])
	}
}

func nest(n int) any {
	var v any = "leaf"
	for range n {
		v = map[string]any{"x": v}
	}
	return v
}

func TestSecretKeys(t *testing.T) {
	for k, want := range map[string]bool{
		"authorization": true, "Proxy-Authorization": true, "Set-Cookie": true, "COLUMN_APP_TOKEN": true, "awsAccessKeyId": true,
		"aws_secret_access_key": true, "X-Amz-Security-Token": true, "token": true, "tokens": false, "tag": false, "id": false,
	} {
		if secretKey(k) != want {
			t.Errorf("secretKey(%q) = %v", k, !want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for _, s := range levelNames {
		if l, ok := ParseLevel(s); !ok || l.String() != s {
			t.Errorf("ParseLevel(%q) = %v %v", s, l, ok)
		}
	}
	if _, ok := ParseLevel("trace"); ok {
		t.Error("trace accepted")
	}
}

func TestErrorDetailOfAnError(t *testing.T) {
	line := string(goldenStamp.line(Event{Payload: Lifecycle{Phase: "x", Detail: errors.New("Bearer tok failed")}}))
	if !strings.Contains(line, `"detail":{"message":"Bearer <redacted> failed"}`) {
		t.Error(line)
	}
}
