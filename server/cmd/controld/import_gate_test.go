package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nickstrad/mansio/server/internal/control"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The import path onto Postgres placement: `dbtool import` without
// -placement-dir writes generation 1 and the manifest only; registering the app's schema and creating the
// database at controld places it at generation 0, and the owner's claim adopts the imported manifest. The
// imported documents are then served (with their versions) and the database takes writes. The source file is a
// real one: another tenant's database of the same app, backed up and restored with dbtool. Gated like the
// other gates (MANSIO_PG_URL, COLDLOAD_S3).
//
//	MANSIO_PG_URL='postgres://mansio:mansio-dev@localhost:5433/mansio?sslmode=disable' COLDLOAD_S3=localhost:8333 \
//	  go test -count=1 -run TestImportAdoptGate -v ./cmd/controld
func TestImportAdoptGate(t *testing.T) {
	c := newCluster(t)
	c.locator, c.schemas, c.controldAddr = true, true, freeAddr(t)
	c.startAgent("vm-a")
	c.startAgent("vm-b")
	c.startControld()
	waitListening(t, c)
	fp := schemareg.Fingerprint([]byte(jepsenSchema))
	if code := c.call("PUT", "/v1/apps/notes/schema", map[string]string{"fingerprint": fp, "schema": jepsenSchema}, nil); code != 200 {
		t.Fatalf("register: %d", code)
	}
	keyOf := func(tenant string) string {
		var k map[string]string
		if code := c.call("POST", "/v1/tenants/"+tenant+"/api-keys", map[string]string{"principal": "app"}, &k); code != 200 {
			t.Fatalf("api key %s: %d", tenant, code)
		}
		return k["key"]
	}
	ops := func(apiKey, tenant, kind, rid string, ops ...map[string]any) (int, map[string]any) {
		t.Helper()
		_, _, r := c.resolveApp(apiKey, tenant, "notes")
		body := map[string]any{"v": 1, "db": map[string]string{"tenant": tenant, "app": "notes"}, "kind": kind, "schema": fp, "ops": ops}
		if rid != "" {
			body["requestId"] = rid
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, "http://"+r.Endpoint+"/v1/ops", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+r.Ticket)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	const doc = "01900000-0000-7000-8000-000000000001"

	// The source: acme:notes with one document patched once (version 2), written back.
	acme := keyOf("acme")
	if code := c.call("POST", "/v1/apps/notes/databases", map[string]string{"tenant": "acme"}, nil); code != 200 {
		t.Fatalf("create acme:notes: %d", code)
	}
	waitFor(t, "acme insert", func() bool {
		code, _ := ops(acme, "acme", "mutation", "ins-1", map[string]any{"op": "insert", "table": "registers", "id": doc, "value": map[string]any{"value": 41}})
		return code == 200
	})
	if code, out := ops(acme, "acme", "mutation", "patch-1", map[string]any{"op": "patch", "table": "registers", "id": doc, "value": map[string]any{"value": 42}}); code != 200 {
		t.Fatalf("patch: %d %v", code, out)
	}
	p, err := c.pl.Get(t.Context(), "acme:notes")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := http.Post("http://"+c.agents[p.Owner].admin+"/admin/backup?key=acme:notes", "", nil); err != nil || res.StatusCode != 200 {
		t.Fatalf("backup: %v %v", res, err)
	}

	// dbtool: restore acme's file, import it as globex's (no placement row).
	dbtool := filepath.Join(c.dir, "dbtool")
	if out, err := exec.Command("go", "build", "-o", dbtool, "../dbtool").CombinedOutput(); err != nil {
		t.Fatalf("build dbtool: %v\n%s", err, out)
	}
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(dbtool, append([]string{"-endpoint", c.s3, "-bucket", "coldload", "-root", c.prefix}, args...)...)
		cmd.Env = append(os.Environ(), "DBTOOL_S3_ACCESS_KEY=agent", "DBTOOL_S3_SECRET_KEY=agent-secret-dev")
		out, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("dbtool %v: %v\n%s%s", args, err, out, stderr)
		}
		return out
	}
	file := filepath.Join(c.dir, "acme-notes.db")
	run("restore", "acme:notes", file)
	schemaFile := filepath.Join(c.dir, "notes.json")
	os.WriteFile(schemaFile, []byte(jepsenSchema), 0o644)
	var imported struct {
		Imported string `json:"imported"`
		Manifest struct {
			Generation uint64 `json:"generation"`
		} `json:"manifest"`
		Placement any `json:"placement"`
	}
	if err := json.Unmarshal(run("import", file, "-tenant", "globex", "-app", "notes", "-schema", schemaFile), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.Imported != file || imported.Manifest.Generation != 1 || imported.Placement != nil {
		t.Fatalf("import: %+v", imported)
	}
	if _, err := c.pl.Get(t.Context(), "globex:notes"); err == nil {
		t.Fatal("import without -placement-dir wrote a placement row")
	}

	// controld creates the database; the owner's claim adopts the imported generation.
	var d control.Database
	if code := c.call("POST", "/v1/apps/notes/databases", map[string]string{"tenant": "globex"}, &d); code != 200 || d.State != control.StateActive {
		t.Fatalf("create globex:notes: %d %+v", code, d)
	}
	globex := keyOf("globex")
	code, out := ops(globex, "globex", "query", "", map[string]any{"op": "get", "table": "registers", "id": doc})
	got, _ := out["result"].([]any)
	if code != 200 || len(got) != 1 {
		t.Fatalf("read the imported document: %d %v", code, out)
	}
	if m, _ := got[0].(map[string]any); m["value"] != 42.0 || m["_version"] != 2.0 {
		t.Fatalf("imported document: %v", got[0])
	}
	if code, out := ops(globex, "globex", "mutation", "patch-2", map[string]any{"op": "patch", "table": "registers", "id": doc,
		"value": map[string]any{"value": 43}, "expect": map[string]any{"version": 2}}); code != 200 {
		t.Fatalf("write after the import: %d %v", code, out)
	}
	// The request id of the source's patch came along in the file: a replay answers duplicate, nothing runs.
	if code, out := ops(globex, "globex", "mutation", "patch-1", map[string]any{"op": "patch", "table": "registers", "id": doc,
		"value": map[string]any{"value": 0}}); code != 200 || out["duplicate"] != true {
		t.Fatalf("replayed request id: %d %v", code, out)
	}
}

// resolveApp resolves (tenant, app) with an API key; it returns the status, the Retry-After header and the route.
func (c *cluster) resolveApp(apiKey, tenant, app string) (int, string, route) {
	c.t.Helper()
	var r route
	var code int
	var retry string
	waitFor(c.t, "resolve "+tenant+":"+app, func() bool {
		b, _ := json.Marshal(map[string]any{"tenant": tenant, "app": app})
		req, _ := http.NewRequest(http.MethodPost, c.api+"/v1/resolve", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer res.Body.Close()
		r = route{}
		json.NewDecoder(res.Body).Decode(&r)
		code, retry = res.StatusCode, res.Header.Get("Retry-After")
		return code == 200
	})
	return code, retry, r
}
