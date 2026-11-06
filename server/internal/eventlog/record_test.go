package eventlog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The goldens are core/events.ts toJsonLine's output for the same records (generated with deno; see
// TestLineMatchesTS). A difference is a format change the TS ingest may not read the same way.
var goldens = []struct {
	name string
	ev   Event
	want string
}{
	{"http with redacted detail", Event{Level: Warn, TraceID: "t-1", AppID: "tic-tac-toe", Payload: HTTP{
		Method: "POST", Path: "/v1/ops", HandlerKind: "mutation", Name: "makeMove", Status: 409, Duration: 1500 * time.Microsecond,
		BytesIn: 120, BytesOut: 77, ErrorTag: "ConflictError", ErrorDetail: map[string]any{
			"tag": "ConflictError", "message": "line1\nline2 Bearer abc.def",
			"fields": map[string]any{"Authorization": "Bearer x", "x-api-token": "s", "nested": []any{1, "Bearer zz", nil, true}},
		}}},
		`{"eventId":"e1","ts":"2026-11-06T10:11:12.345Z","level":"warn","role":"server","pid":42,"traceId":"t-1","spanId":null,"parentSpanId":null,"appId":"tic-tac-toe","kind":"http","method":"POST","path":"/v1/ops","handlerKind":"mutation","name":"makeMove","status":409,"durationMs":1.5,"bytesIn":120,"bytesOut":77,"errorTag":"ConflictError","errorDetail":{"fields":{"Authorization":"<redacted>","nested":[1,"Bearer <redacted>",null,true],"x-api-token":"<redacted>"},"message":"line1 line2 Bearer <redacted>","tag":"ConflictError"}}`},
	{"handler", Event{TraceID: "t-1", SpanID: "s1", ParentSpanID: "p1", AppID: "a", Payload: Handler{
		HandlerKind: "query", Name: "getGame", Outcome: "ok", Duration: 250 * time.Microsecond}},
		`{"eventId":"e1","ts":"2026-11-06T10:11:12.345Z","level":"info","role":"server","pid":42,"traceId":"t-1","spanId":"s1","parentSpanId":"p1","appId":"a","kind":"handler","handlerKind":"query","name":"getGame","outcome":"ok","durationMs":0.25,"errorTag":null,"errorDetail":null}`},
	{"sql never carries bind values", Event{TraceID: "t-1", SpanID: "s1", AppID: "a", Payload: SQL{
		Engine: "sqlite", Dialect: "sqlite", StatementKind: "select", TableName: "games", SQLText: `SELECT * FROM "games" WHERE "_id" = ?`,
		BindCount: 1, Duration: 12 * time.Microsecond, RowCount: 1, Outcome: "ok"}},
		`{"eventId":"e1","ts":"2026-11-06T10:11:12.345Z","level":"info","role":"server","pid":42,"traceId":"t-1","spanId":"s1","parentSpanId":null,"appId":"a","kind":"sql","engine":"sqlite","dialect":"sqlite","statementKind":"select","path":null,"tableName":"games","sqlText":"SELECT * FROM \"games\" WHERE \"_id\" = ?","bindCount":1,"bindValues":null,"durationMs":0.012,"rowCount":1,"outcome":"ok","errorTag":null,"errorDetail":null}`},
	{"tx", Event{TraceID: "t-1", SpanID: "s1", AppID: "a", Payload: Tx{Engine: "sqlite", Outcome: "rollback", Operations: 3,
		Duration: 2 * time.Millisecond, QueueWait: 500 * time.Microsecond, ErrorTag: "StorageError", ErrorDetail: map[string]any{"reason": "x"}}},
		`{"eventId":"e1","ts":"2026-11-06T10:11:12.345Z","level":"info","role":"server","pid":42,"traceId":"t-1","spanId":"s1","parentSpanId":null,"appId":"a","kind":"tx","engine":"sqlite","outcome":"rollback","operations":3,"durationMs":2,"queueWaitMs":0.5,"errorTag":"StorageError","errorDetail":{"reason":"x"}}`},
	{"queue drop report", Event{Level: Warn, TraceID: SinkTraceID, Payload: Queue{Queue: "events", Depth: 10, Cap: 10, Event: "dropped", Dropped: 3}},
		`{"eventId":"e1","ts":"2026-11-06T10:11:12.345Z","level":"warn","role":"server","pid":42,"traceId":"event-sink","spanId":null,"parentSpanId":null,"appId":null,"kind":"queue","queue":"events","depth":10,"cap":10,"event":"dropped","dropped":3}`},
	{"lifecycle with S3 secrets", Event{TraceID: "t-1", Payload: Lifecycle{Phase: "startup", Detail: map[string]any{
		"url": "https://s3/x?X-Amz-Signature=abc&X-Amz-Credential=def&ok=1", "AWS_SECRET_ACCESS_KEY": "k", "sessionToken": "t",
		"list": []any{"a b", map[string]any{"Cookie": "c"}}}}},
		`{"eventId":"e1","ts":"2026-11-06T10:11:12.345Z","level":"info","role":"server","pid":42,"traceId":"t-1","spanId":null,"parentSpanId":null,"appId":null,"kind":"lifecycle","phase":"startup","detail":{"AWS_SECRET_ACCESS_KEY":"<redacted>","list":["a b",{"Cookie":"<redacted>"}],"sessionToken":"<redacted>","url":"https://s3/x?X-Amz-Signature=<redacted>&X-Amz-Credential=<redacted>&ok=1"}}`},
	{"backup with absent optionals", Event{TraceID: "t-1", AppID: "a", Payload: Backup{Phase: "publish-latest", Emitter: "backups", RunID: "r1",
		Bytes: -1, Attempts: -1, Duration: 3 * time.Millisecond, Outcome: "ok"}},
		`{"eventId":"e1","ts":"2026-11-06T10:11:12.345Z","level":"info","role":"server","pid":42,"traceId":"t-1","spanId":null,"parentSpanId":null,"appId":"a","kind":"backup","phase":"publish-latest","emitter":"backups","runId":"r1","key":null,"bytes":null,"sha256":null,"attempts":null,"durationMs":3,"outcome":"ok","errorTag":null,"errorDetail":null}`},
}

var goldenStamp = stamp{EventID: "e1", TS: time.Date(2026, 11, 6, 10, 11, 12, 345e6, time.UTC), Role: "server", PID: 42}

// TestLineMatchesTS: every kind serializes byte for byte as core/events.ts toJsonLine does, redaction included.
func TestLineMatchesTS(t *testing.T) {
	for _, g := range goldens {
		t.Run(g.name, func(t *testing.T) {
			if got := string(goldenStamp.line(g.ev)); got != g.want {
				t.Fatalf("line differs from core/events.ts\ngot:  %s\nwant: %s", got, g.want)
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
