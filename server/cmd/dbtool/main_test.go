package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/objstore/s3test"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

var ctx = context.Background()

// rig is one backend: where dbtool points, plus direct handles to seed and break things.
type rig struct {
	t        *testing.T
	endpoint string
	bucket   string
	root     string
	env      map[string]string
	st       *objstore.Store
	d        *objstore.Durable
}

func fakeRig(t *testing.T) *rig {
	fake := s3test.New()
	t.Cleanup(fake.Close)
	return newRig(t, fake.Endpoint(), "bkt", "a", "b")
}

func newRig(t *testing.T, endpoint, bucket, access, secret string) *rig {
	t.Helper()
	st, err := objstore.New(objstore.Config{Endpoint: endpoint, AccessKey: access, SecretKey: secret, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	root := "test-" + logx.NewID() + "/"
	t.Cleanup(func() { st.DeletePrefix(context.Background(), root) })
	return &rig{t: t, endpoint: endpoint, bucket: bucket, root: root, st: st, d: objstore.NewDurable(st, root),
		env: map[string]string{"DBTOOL_S3_ACCESS_KEY": access, "DBTOOL_S3_SECRET_KEY": secret}}
}

// run runs dbtool against the rig and returns the exit code, stdout and stderr.
func (r *rig) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	full := append([]string{"-endpoint", r.endpoint, "-bucket", r.bucket, "-root", r.root}, args...)
	code := realMain(ctx, full, func(k string) string { return r.env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func (r *rig) mustRun(want int, args ...string) string {
	r.t.Helper()
	code, out, errOut := r.run(args...)
	if code != want {
		r.t.Fatalf("dbtool %v = %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errOut)
	}
	return out + errOut
}

// seed uploads generations 1..n of key at epoch 1 (contents "<key> g<i>"), waits so the publish is strictly
// later on a 1 s LastModified clock, and publishes generation cur. Returns the manifests.
func (r *rig) seed(key string, n int, cur int) []durable.Manifest {
	r.t.Helper()
	var ms []durable.Manifest
	for i := 1; i <= n; i++ {
		data := []byte(fmt.Sprintf("%s g%d", key, i))
		p := filepath.Join(r.t.TempDir(), "g")
		os.WriteFile(p, data, 0o644)
		m := durable.NextManifest(key, uint64(i), 1, durabletest.DigestOf(data), time.Now())
		if err := r.d.PutGeneration(ctx, m, p); err != nil {
			r.t.Fatal(err)
		}
		ms = append(ms, m)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := r.d.PublishManifest(ctx, ms[cur-1], durable.NoVersion); err != nil {
		r.t.Fatal(err)
	}
	return ms
}

func TestUsage(t *testing.T) {
	r := fakeRig(t)
	for _, args := range [][]string{
		{}, {"nope"}, {"verify"}, {"verify", "a", "b"}, {"verify", "/bad"},
		{"gc"}, {"gc", "-key", "a", "-all"}, {"gc", "-shard", "zz"},
		{"rebuild-manifest", "a"}, {"rebuild-manifest", "-gen", "1", "a"},
	} {
		if code, _, _ := r.run(args...); code != exitUsage {
			t.Errorf("dbtool %v = %d, want %d", args, code, exitUsage)
		}
	}
	r.env = nil
	if code, _, errOut := r.run("verify", "a"); code != exitUsage || !strings.Contains(errOut, "DBTOOL_S3_ACCESS_KEY") {
		t.Errorf("no credentials = %d %q", code, errOut)
	}
}

// OS-12 acceptance, against the in-process S3 always and SeaweedFS when COLDLOAD_S3 is set: verify finds a
// corrupt generation; a lost manifest is rebuilt from the generation's metadata and the database activates;
// rebuild never overwrites a manifest the operator did not see, and a rollback needs a higher epoch; gc lists
// and deletes old orphans.
func TestRecovery(t *testing.T) {
	t.Run("in-process S3", func(t *testing.T) { recovery(t, fakeRig(t)) })
	t.Run("SeaweedFS", func(t *testing.T) {
		ep := os.Getenv("COLDLOAD_S3")
		if ep == "" {
			t.Skip("set COLDLOAD_S3=host:port to run against a real S3")
		}
		recovery(t, newRig(t, ep, "coldload", "coldload", "coldload-secret"))
	})
}

func recovery(t *testing.T, r *rig) {
	// Corrupt generation.
	ms := r.seed("t/corrupt", 1, 1)
	if out := r.mustRun(exitOK, "verify", "t/corrupt"); !strings.Contains(out, "OK") {
		t.Fatalf("verify: %s", out)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("t/corrupt gX"), 0o644) // same size, other bytes, no metadata
	if _, err := r.st.PutFile(ctx, r.root+ms[0].Object, bad); err != nil {
		t.Fatal(err)
	}
	if out := r.mustRun(exitError, "verify", "t/corrupt"); !strings.Contains(out, "log ID") {
		t.Fatalf("verify of a corrupt generation: %s", out)
	}

	// Lost manifest, then rebuild.
	ms = r.seed("t/lost", 5, 4) // 5 is an orphan
	if out := r.mustRun(exitOK, "inspect", "t/lost"); !strings.Contains(out, "CURRENT") || strings.Count(out, "meta: key=t/lost") != 5 {
		t.Fatalf("inspect: %s", out)
	}
	if err := r.st.Delete(ctx, []string{r.root + durable.ManifestObject("t/lost")}); err != nil {
		t.Fatal(err)
	}
	if out := r.mustRun(exitOK, "inspect", "t/lost"); !strings.Contains(out, "manifest  none") {
		t.Fatalf("inspect without a manifest: %s", out)
	}
	r.mustRun(exitError, "rebuild-manifest", "-gen", "9", "-epoch", "1", "t/lost") // no such generation
	r.mustRun(exitOK, "rebuild-manifest", "-gen", "4", "-epoch", "1", "t/lost")
	r.mustRun(exitOK, "verify", "t/lost")
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: r.d})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.GetAt(ctx, "t/lost", 1); err != nil {
		t.Fatalf("activation after the rebuild: %v", err)
	}
	if b, _ := os.ReadFile(h.LocalPath("t/lost")); string(b) != "t/lost g4" {
		t.Fatalf("activated %q", b)
	}
	h.Passivate(ctx, "t/lost")

	// A manifest exists now: rebuild needs the version the operator saw, and a rollback a higher epoch.
	l, err := r.d.ListKey(ctx, "t/lost")
	if err != nil || l.Manifest == nil {
		t.Fatal(l, err)
	}
	v := string(l.Manifest.Version)
	r.mustRun(exitError, "rebuild-manifest", "-gen", "3", "-epoch", "1", "t/lost")
	r.mustRun(exitError, "rebuild-manifest", "-gen", "3", "-epoch", "2", "-if-version", "nope", "t/lost")
	r.mustRun(exitError, "rebuild-manifest", "-gen", "3", "-epoch", "1", "-if-version", v, "t/lost")
	r.mustRun(exitOK, "rebuild-manifest", "-gen", "3", "-epoch", "2", "-if-version", v, "t/lost")
	if m, _, err := r.d.ReadManifest(ctx, "t/lost"); err != nil || m.Generation != 3 || m.Epoch != 2 {
		t.Fatalf("after the rollback: %+v, %v", m, err)
	}

	// GC: the default grace keeps everything young; a 1 ns grace lists, then deletes, what PlanGC allows.
	if out := r.mustRun(exitOK, "orphans", "t/lost"); !strings.Contains(out, "would delete 0 generations") {
		t.Fatalf("orphans inside the grace window: %s", out)
	}
	time.Sleep(1100 * time.Millisecond) // the rollback's publish is the manifest's last rewrite
	out := r.mustRun(exitOK, "-grace", "1ns", "-keep", "1", "gc", "-dry-run", "-key", "t/lost")
	if !strings.Contains(out, "would delete 3 generations") { // keep 3 (current) and 2; delete 1, 4, 5
		t.Fatalf("gc dry run: %s", out)
	}
	if l, _ := r.d.ListKey(ctx, "t/lost"); len(l.Generations) != 5 {
		t.Fatalf("a dry run deleted: %d left", len(l.Generations))
	}
	out = r.mustRun(exitOK, "-grace", "1ns", "-keep", "1", "gc", "-all")
	if !strings.Contains(out, "deleted 3 generations") {
		t.Fatalf("gc -all: %s", out)
	}
	if l, _ := r.d.ListKey(ctx, "t/lost"); len(l.Generations) != 2 {
		t.Fatalf("after gc: %+v", l.Generations)
	}
	r.mustRun(exitOK, "verify", "t/lost")
}

// A manifest that does not decode can be replaced only on the version inspect listed.
func TestRebuildReplacesAnUnreadableManifest(t *testing.T) {
	r := fakeRig(t)
	r.seed("t/bad", 2, 2)
	junk := filepath.Join(t.TempDir(), "junk")
	os.WriteFile(junk, []byte("{"), 0o644)
	if _, err := r.st.PutFile(ctx, r.root+durable.ManifestObject("t/bad"), junk); err != nil {
		t.Fatal(err)
	}
	if out := r.mustRun(exitOK, "inspect", "t/bad"); !strings.Contains(out, "UNREADABLE") {
		t.Fatalf("inspect: %s", out)
	}
	r.mustRun(exitError, "verify", "t/bad")
	r.mustRun(exitError, "rebuild-manifest", "-gen", "2", "-epoch", "1", "t/bad")
	l, _ := r.d.ListKey(ctx, "t/bad")
	r.mustRun(exitOK, "rebuild-manifest", "-gen", "2", "-epoch", "1", "-if-version", string(l.Manifest.Version), "t/bad")
	r.mustRun(exitOK, "verify", "t/bad")
}
