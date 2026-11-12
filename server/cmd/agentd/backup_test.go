package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/objstore/s3test"
)

// Backup: a warm, dirty key is written back and keeps serving; the answer is the manifest now stored
// (same generation, object, sha256); a second backup with nothing new uploads nothing and answers the same
// generation; a write then a backup publishes a newer one. A key not active here, and bad keys, are refused.
func TestAdminBackupWritesBackAndAnswersTheManifest(t *testing.T) {
	r := newDurableRig(t, adminExtra)
	a := r.start(t)
	a.claim(t, "db-1")
	writeRows(t, a, "db-1", 1, 0, 3)

	out := a.mustAdmin(t, "/admin/backup?key=db-1")
	m := r.manifest(t, "db-1")
	if out["wrote"] != true || out["key"] != "db-1" || out["generation"] != float64(m.Generation) || m.Generation < 1 ||
		out["object"] != m.Object || out["sha256"] != m.SHA256 || out["size"] != float64(m.Size) || out["version"] == "" {
		t.Fatalf("backup answer %v, stored manifest %+v", out, m)
	}
	if _, h := a.control(t, healthPath, nil); h["active"].(float64) != 1 {
		t.Fatalf("backup dropped the warm copy: %v", h)
	}
	if again := a.mustAdmin(t, "/admin/backup?key=db-1"); again["wrote"] != false || again["generation"] != out["generation"] ||
		again["version"] != out["version"] {
		t.Fatalf("clean backup %v, want the same manifest and nothing written (first %v)", again, out)
	}
	writeRows(t, a, "db-1", 1, 3, 4)
	if next := a.mustAdmin(t, "/admin/backup?key=db-1"); next["wrote"] != true || next["generation"].(float64) <= out["generation"].(float64) {
		t.Fatalf("backup after a write %v, want a newer generation than %v", next, out["generation"])
	}
	if n := count(t, a, "db-1", 1); n != 4 {
		t.Fatalf("after backups: %d rows", n)
	}

	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{"/admin/backup?key=db-2", "wrong_owner", http.StatusConflict}, // never claimed here
		{"/admin/backup?key=", "bad_request", http.StatusBadRequest},
		{"/admin/backup", "bad_request", http.StatusBadRequest},
	} {
		if code, out := adminCall(t, a.admin, tc.path); code != tc.status || out["code"] != tc.want {
			t.Errorf("%s: %d %v, want %d %s", tc.path, code, out, tc.status, tc.want)
		}
	}
	if !strings.Contains(r.log(), "ADMIN.BACKUP") {
		t.Error("backup not logged")
	}
}

// Durable mode with a local schema registry (schemas_dir): the first /v1/ops request to a new key
// creates its tables from the registered schema, and the documents survive a write-back and a cold start.
func TestDurableOpsWithSchemasDir(t *testing.T) {
	r := newDurableRig(t, adminExtra+`,"schemas_dir":"schemas"`)
	os.MkdirAll(filepath.Join(r.dir, "schemas", "ttt"), 0o755)
	os.WriteFile(filepath.Join(r.dir, "schemas", "ttt", "v1.json"), []byte(opsSchema+"\n"), 0o644)
	s, e := docexec.Decode(opsSchema)
	if e != nil {
		t.Fatal(e.Message())
	}
	tok := iss.Mint(t, iss.Claims("acme:ttt", "vm-1", time.Now(), time.Minute))
	post := func(a *running, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+a.addr+front.OpsPath, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	env := `{"v":1,"db":{"tenant":"acme","app":"ttt"},"schema":"` + s.Fingerprint + `",`
	insert := env + `"kind":"mutation","requestId":"m1","ops":[{"op":"insert","table":"games","value":{"player":"amy","moves":1}}]}`
	read := env + `"kind":"query","ops":[{"op":"select","table":"games","filters":[],"limit":10,"aggregate":{"kind":"count"}}]}`

	a := r.start(t)
	a.claim(t, "acme:ttt")
	// The insert's sequence: on a durable agent the first one is the incarnation's floor (spec §6.5), not 1.
	var inserted struct{ Seq int64 }
	if code, body := post(a, insert); code != http.StatusOK || json.Unmarshal([]byte(body), &inserted) != nil || inserted.Seq < 1 {
		t.Fatalf("insert: %d %s", code, body)
	}
	if out := a.mustAdmin(t, "/admin/backup?key=acme:ttt"); out["generation"].(float64) < 1 {
		t.Fatalf("backup %v", out)
	}
	if code := a.stop(); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, a.stderr.String())
	}
	b := r.start(t)
	waitReady(t, b.life)
	if code, body := post(b, read); code != http.StatusOK || body != fmt.Sprintf(`{"result":[1],"seq":%d}`+"\n", inserted.Seq) {
		t.Fatalf("cold read: %d %q", code, body)
	}
}

// A backup answers only this owner's manifest: once a higher epoch stamped it (this agent superseded with a
// clean warm copy, so the write-back has nothing to upload), it is 409 fenced. An S3 failure is 503.
func TestAdminBackupRefusals(t *testing.T) {
	r := newDurableRig(t, adminExtra)
	a := r.start(t)
	a.claim(t, "db-1")
	a.claim(t, "db-2")
	writeRows(t, a, "db-1", 1, 0, 1)
	writeRows(t, a, "db-2", 1, 0, 1)
	a.mustAdmin(t, "/admin/backup?key=db-1") // clean from here on

	st, err := objstore.New(objstore.Config{Endpoint: r.endpoint, AccessKey: r.access, SecretKey: r.secret, Bucket: r.bucket})
	if err != nil {
		t.Fatal(err)
	}
	m := r.manifest(t, "db-1")
	if _, _, err := durable.Stamp(context.Background(), objstore.NewDurable(st, r.prefix), "db-1", 2, m.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code, out := adminCall(t, a.admin, "/admin/backup?key=db-1"); code != http.StatusConflict || out["code"] != "fenced" {
		t.Fatalf("backup after a higher epoch stamped: %d %v", code, out)
	}

	r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: http.StatusServiceUnavailable})
	code, out := adminCall(t, a.admin, "/admin/backup?key=db-2")
	if code != http.StatusServiceUnavailable || out["code"] != "unavailable" {
		t.Fatalf("backup with S3 down: %d %v", code, out)
	}
	r.fake.Heal()
}
