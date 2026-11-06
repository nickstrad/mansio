package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// opsSchema is the app's registered schema, as a schemas_dir file holds it (with an editor's newline).
const opsSchema = `{"games":{"fields":{"player":{"node":{"kind":"string"}},"moves":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]}}`

// Single-node agentd end to end over HTTP (IR-2b): a static-mode agent with a schemas_dir and an empty
// database file for (tenant, app) acme:ttt. The first /v1/ops request creates the tables from the registered
// schema; mutations, a duplicate, a query and a drifted fingerprint follow; the data survives a restart.
func TestOpsEndToEnd(t *testing.T) {
	cfgPath := fixture(t, nil, `,"schemas_dir":"schemas"`)
	dir := filepath.Dir(cfgPath)
	os.WriteFile(filepath.Join(dir, "dbs", "acme:ttt.db"), nil, 0o644) // a new, empty database
	os.MkdirAll(filepath.Join(dir, "schemas", "ttt"), 0o755)
	os.WriteFile(filepath.Join(dir, "schemas", "ttt", "v1.json"), []byte(opsSchema+"\n"), 0o644)
	s, e := docexec.Decode(opsSchema)
	if e != nil {
		t.Fatal(e.Message())
	}
	fp := s.Fingerprint

	start := func() (addr string, stop func()) {
		addrFile := filepath.Join(t.TempDir(), "addr")
		ctx, cancel := context.WithCancel(context.Background())
		var stderr syncBuf
		done := make(chan int)
		go func() { done <- realMain(ctx, []string{"-config", cfgPath, "-addr-file", addrFile}, &stderr) }()
		addr = waitAddr(t, addrFile, done)
		waitReady(t, lifecycleAddr(t, addrFile))
		return addr, func() {
			cancel()
			if code := <-done; code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
		}
	}
	tok := iss.Mint(t, iss.Claims("acme:ttt", "vm-1", time.Now(), time.Minute))
	post := func(addr, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+front.OpsPath, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set(front.TimeoutHeader, "5000")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSuffix(string(b), "\n")
	}
	env := func(kind, rid, schema, ops string) string {
		if rid != "" {
			rid = `"requestId":"` + rid + `",`
		}
		return `{"v":1,"db":{"tenant":"acme","app":"ttt"},"kind":"` + kind + `",` + rid + `"schema":"` + schema + `","ops":` + ops + `}`
	}

	addr, stop := start()
	create := env("mutation", "m1", fp, `[{"op":"insert","table":"games","value":{"player":"amy","moves":0}},`+
		`{"op":"patch","table":"games","id":{"$ref":0},"value":{"moves":1},"expect":{"version":1}}]`)
	code, body := post(addr, create)
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	v, _ := ir.Parse(body)
	result := v.(*ir.Object).Value("result").([]any)
	id := result[0].(string)
	if !ir.UUIDv7.MatchString(id) || ir.Stringify(result[1]) != `{"_version":2}` {
		t.Fatalf("create: %s", body)
	}
	if code, dup := post(addr, create); code != 200 || dup != strings.TrimSuffix(body, "}")+`,"duplicate":true}` {
		t.Fatalf("duplicate: %d %s", code, dup)
	}
	read := env("query", "", fp, `[{"op":"get","table":"games","id":"`+id+`"},{"op":"select","table":"games","filters":[{"field":"player","op":"eq","value":"amy"}],"limit":10,"aggregate":{"kind":"count"}}]`)
	if code, got := post(addr, read); code != 200 || !strings.Contains(got, `"moves":1`) || !strings.HasSuffix(got, `,1]}`) {
		t.Fatalf("query: %d %s", code, got)
	}
	conflict := env("mutation", "m2", fp, `[{"op":"patch","table":"games","id":"`+id+`","value":{"moves":9},"expect":{"version":1}}]`)
	if code, got := post(addr, conflict); code != 409 || !strings.Contains(got, `"tag":"ConflictError"`) || !strings.Contains(got, `"op":0`) {
		t.Fatalf("conflict: %d %s", code, got)
	}
	drifted := "sha256:" + strings.Repeat("ab", 32)
	if code, got := post(addr, env("query", "", drifted, `[{"op":"get","table":"games","id":"x"}]`)); code != 409 ||
		!strings.Contains(got, `"tag":"SchemaDriftError"`) || !strings.Contains(got, `"expected":"`+fp+`"`) {
		t.Fatalf("drift: %d %s", code, got)
	}
	stop()

	addr, stop = start() // the database file kept the documents and the request ids
	defer stop()
	if code, got := post(addr, read); code != 200 || !strings.Contains(got, `"moves":1`) {
		t.Fatalf("after restart: %d %s", code, got)
	}
	if code, dup := post(addr, create); code != 200 || !strings.Contains(dup, `"duplicate":true`) {
		t.Fatalf("duplicate after restart: %d %s", code, dup)
	}
}

// schemas_dir must hold valid schemas in fingerprint form under app-named directories.
func TestSchemaDir(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	write("ttt/a.json", opsSchema+"\r\n")
	src, err := loadSchemaDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := docexec.Decode(opsSchema)
	if text, err := src.Schema(context.Background(), "ttt", s.Fingerprint); err != nil || text != opsSchema {
		t.Fatalf("Schema = %q, %v", text, err)
	}
	if _, err := src.Schema(context.Background(), "other", s.Fingerprint); err == nil {
		t.Fatal("another app's schema")
	}
	for name, text := range map[string]string{"Bad.App/a.json": opsSchema, "ttt/b.json": " " + opsSchema, "ttt/c.json": `{"t":{}}`} {
		d := t.TempDir()
		p := filepath.Join(d, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
		if _, err := loadSchemaDir(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if src, err := loadSchemaDir(""); src != nil || err != nil {
		t.Fatalf("no dir: %v %v", src, err)
	}
}
