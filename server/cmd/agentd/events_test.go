package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/eventlog"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// records reads every event log record under dir/server.
func records(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	paths, _ := filepath.Glob(filepath.Join(dir, "server", "*", "events-*.jsonl"))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var r map[string]any
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			out = append(out, r)
		}
		f.Close()
	}
	return out
}

// TestEventLogEndToEnd: an agent with event_log writes, per /v1/ops request, the http, handler, tx and sql
// records under one trace, plus lifecycle startup and shutdown, with no token in the log.
func TestEventLogEndToEnd(t *testing.T) {
	cfgPath := fixture(t, nil, `,"schemas_dir":"schemas","event_log":{"dir":"logs","trace_sql":true}`)
	dir := filepath.Dir(cfgPath)
	os.WriteFile(filepath.Join(dir, "dbs", "acme:ttt.db"), nil, 0o644)
	os.MkdirAll(filepath.Join(dir, "schemas", "ttt"), 0o755)
	os.WriteFile(filepath.Join(dir, "schemas", "ttt", "v1.json"), []byte(opsSchema), 0o644)
	s, _ := docexec.Decode(opsSchema)

	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfgPath, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)
	waitReady(t, lifecycleAddr(t, addrFile))
	tok := iss.Mint(t, iss.Claims("acme:ttt", "vm-1", time.Now(), time.Minute))
	post := func(trace, handler, body string) (int, http.Header) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.OpsPath, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set(front.TraceHeader, trace)
		req.Header.Set(front.HandlerHeader, handler)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode, res.Header
	}
	ins := `{"v":1,"db":{"tenant":"acme","app":"ttt"},"kind":"mutation","requestId":"m1","schema":"` + s.Fingerprint +
		`","ops":[{"op":"insert","table":"games","value":{"player":"amy","moves":0}}]}`
	if code, h := post("trace-a", "createGame", ins); code != 200 || h.Get(front.TraceHeader) != "trace-a" {
		t.Fatalf("insert: %d %v", code, h)
	}
	drift := strings.Replace(ins, s.Fingerprint, "sha256:"+strings.Repeat("0", 64), 1)
	if code, _ := post("trace-b", "createGame", drift); code != 409 {
		t.Fatalf("drift: %d", code)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	recs := records(t, filepath.Join(dir, "logs"))
	byTrace := map[string]map[string][]map[string]any{}
	for _, r := range recs {
		if r["role"] != "server" || len(r["eventId"].(string)) != 36 {
			t.Errorf("envelope: %v", r)
		}
		tr, _ := r["traceId"].(string)
		if byTrace[tr] == nil {
			byTrace[tr] = map[string][]map[string]any{}
		}
		byTrace[tr][r["kind"].(string)] = append(byTrace[tr][r["kind"].(string)], r)
	}
	a := byTrace["trace-a"]
	if len(a["http"]) != 1 || len(a["handler"]) != 1 || len(a["tx"]) != 1 || len(a["sql"]) == 0 {
		t.Fatalf("trace-a kinds: %v", a)
	}
	h := a["http"][0]
	if h["name"] != "createGame" || h["handlerKind"] != "mutation" || h["status"] != 200.0 || h["appId"] != "ttt" || h["path"] != "/v1/ops" {
		t.Errorf("http: %v", h)
	}
	if a["tx"][0]["outcome"] != "commit" || a["tx"][0]["spanId"] != a["handler"][0]["spanId"] || a["sql"][0]["spanId"] != a["handler"][0]["spanId"] {
		t.Errorf("tx %v handler %v", a["tx"][0], a["handler"][0])
	}
	var insert bool
	for _, q := range a["sql"] {
		if q["statementKind"] == "insert" && q["tableName"] == "games" && q["bindValues"] == nil {
			insert = true
		}
	}
	if !insert {
		t.Errorf("no insert sql record: %v", a["sql"])
	}
	b := byTrace["trace-b"]["http"]
	if len(b) != 1 || b[0]["status"] != 409.0 || b[0]["errorTag"] != "SchemaDriftError" || b[0]["level"] != "warn" {
		t.Errorf("drift http: %v", b)
	}
	life := byTrace[lifecycleTrace]["lifecycle"]
	if len(life) != 2 || life[0]["phase"] != "startup" || life[1]["phase"] != "shutdown" {
		t.Errorf("lifecycle: %v", life)
	}
	raw, _ := json.Marshal(recs)
	if strings.Contains(string(raw), tok) {
		t.Error("the ticket reached the event log")
	}
}

func TestOpsRecordsFailures(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	ids := func() string { return "span" }
	timeout := front.OpsEvent{Start: t0, Duration: 5 * time.Millisecond, Method: "POST", Path: front.OpsPath, TraceID: "t", Handler: "h",
		Out: front.OpsOutput{Status: 504, Code: front.StmtTimeout, Tag: "StatementDeadlineError", Body: []byte(`{"error":{"tag":"StatementDeadlineError","message":"m"}}`)},
		Ops: front.OpsTraceSnapshot{Kind: ir.Query, App: "a", Submitted: t0, Began: t0.Add(time.Millisecond), Ended: t0.Add(4 * time.Millisecond),
			Statements: []front.Statement{{SQL: `SELECT * FROM "g"`, Start: t0, Duration: time.Millisecond}, {SQL: `SELECT * FROM "g"`, Start: t0, Err: errors.New("interrupted")}}}}
	recs := opsRecords(timeout, false, ids)
	kinds := []string{}
	for _, r := range recs {
		kinds = append(kinds, r.Payload.Kind())
	}
	if strings.Join(kinds, ",") != "http,sql,tx,handler" {
		t.Fatalf("kinds %v (without trace_sql only the failed statement)", kinds)
	}
	if q := recs[1].Payload; q.(eventlog.SQL).ErrorTag != "StatementDeadlineError" || q.(eventlog.SQL).Outcome != "error" {
		t.Errorf("sql %+v", q)
	}
	if tx := recs[2].Payload.(eventlog.Tx); tx.Outcome != "interrupted" || tx.QueueWait != time.Millisecond || tx.Duration != 3*time.Millisecond || tx.Operations != 2 {
		t.Errorf("tx %+v", tx)
	}
	if h := recs[0].Payload.(eventlog.HTTP); h.ErrorTag != "StatementDeadlineError" || h.ErrorDetail.(map[string]any)["message"] != "m" || h.HandlerKind != "query" {
		t.Errorf("http %+v", h)
	}

	refused := front.OpsEvent{Start: t0, Method: "POST", Path: front.OpsPath, TraceID: "t", Handler: front.OpsName,
		Out: front.OpsOutput{Status: 401, Code: front.TicketInvalid, Tag: "ticket_invalid", Body: []byte(`{"error":{"tag":"ticket_invalid"}}`)}}
	if recs := opsRecords(refused, true, ids); len(recs) != 1 || recs[0].Payload.(eventlog.HTTP).HandlerKind != "mutation" {
		t.Errorf("an undecoded request is one http record: %+v", recs)
	}
}

func TestStatementKindAndTable(t *testing.T) {
	for sql, want := range map[string][2]string{
		`SELECT * FROM "games" WHERE "_id" = ?`:           {"select", "games"},
		`INSERT INTO "moves" ("_id") VALUES (?)`:          {"insert", "moves"},
		`UPDATE "games" SET "turn" = ?`:                   {"update", "games"},
		`DELETE FROM "moves" WHERE "_id" = ?`:             {"delete", "moves"},
		`CREATE TABLE IF NOT EXISTS "_mansio_schema" (x)`: {"ddl", "_mansio_schema"},
		`WITH x AS (SELECT 1) SELECT count(*) FROM "g"`:   {"select", "g"},
		`pragma user_version`:                             {"pragma", ""},
	} {
		if k, tb := statementKind(sql), tableName(sql); k != want[0] || tb != want[1] {
			t.Errorf("%s: %s %q, want %v", sql, k, tb, want)
		}
	}
}

func TestBackupRecord(t *testing.T) {
	ok := backupRecord("acme:ttt", backupAnswer{Manifest: durable.Manifest{Object: "o/1", Size: 42, SHA256: "ab"}}, nil, time.Millisecond)
	b := ok.Payload.(eventlog.Backup)
	if ok.AppID != "ttt" || b.Phase != "publish-latest" || b.Emitter != "backups" || b.Outcome != "ok" || b.Bytes != 42 || b.Key != "o/1" || b.RunID == "" {
		t.Errorf("ok: %+v %+v", ok, b)
	}
	failed := backupRecord("acme:ttt", backupAnswer{}, &controlError{"wrong_owner", 409, "not active"}, time.Millisecond)
	if b := failed.Payload.(eventlog.Backup); b.Outcome != "error" || b.ErrorTag != "wrong_owner" || failed.Level != eventlog.Warn {
		t.Errorf("failed: %+v", b)
	}
}
