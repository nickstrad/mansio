package vactor_test

import (
	"context"
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

// s3Rig is a durable host over the real S3 adapter and the in-process misbehaving S3 (s3test).
type s3Rig struct {
	h    *vactor.Host
	fake *s3test.Server
	d    *objstore.Durable
}

func newS3Rig(t *testing.T) *s3Rig {
	t.Helper()
	fake := s3test.New()
	t.Cleanup(fake.Close)
	st, err := objstore.New(objstore.Config{Endpoint: fake.Endpoint(), AccessKey: "a", SecretKey: "b", Bucket: "bkt",
		OpTimeout: 2 * time.Second, StallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	d := objstore.NewDurable(st, "")
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: d, CreateMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fake.Heal(); h.Close() })
	return &s3Rig{h: h, fake: fake, d: d}
}

func (r *s3Rig) write(t *testing.T, key string, epoch uint64, v string) {
	t.Helper()
	res, err := r.h.GetAt(ctx, key, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.DB.Query(ctx, vactortest.Put(v)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.WriteBack(ctx, key); err != nil {
		t.Fatalf("WriteBack(%s): %v", key, err)
	}
}

// RI-14 / OS-02 through the real adapter: a stamp's manifest PUT applies but its reply is dropped, or the
// server answers 412 after applying it (what a client-side re-send of a lost reply gets, G2c). Warm and cold
// Stamp both re-read, succeed, and the owner can still write back.
func TestStampLostReplyThroughS3(t *testing.T) {
	for _, mode := range []s3test.Mode{s3test.ApplyThenDropReply, s3test.ApplyThenPreconditionFailed} {
		for _, tc := range []struct {
			name string
			warm bool
		}{{"warm", true}, {"cold", false}} {
			t.Run(mode.String()+"/"+tc.name, func(t *testing.T) {
				r := newS3Rig(t)
				r.write(t, "t/a", 1, "v1")
				if !tc.warm {
					if err := r.h.Passivate(ctx, "t/a"); err != nil {
						t.Fatal(err)
					}
				}
				r.fake.Inject(s3test.Fault{Mode: mode, Method: "PUT", Times: 1})
				m, err := r.h.Stamp(ctx, "t/a", 2, 1)
				if err != nil || m.Epoch != 2 {
					t.Fatalf("Stamp = %+v, %v; the re-read shows it applied", m, err)
				}
				r.write(t, "t/a", 2, "v2")
				if got, _, err := r.d.ReadManifest(ctx, "t/a"); err != nil || got.Epoch != 2 || got.Generation != 2 {
					t.Fatalf("manifest %+v, %v", got, err)
				}
			})
		}
	}
}

// OS-02 through the real adapter: the generation PUT applies but its reply is lost. The HEAD's sha256
// metadata shows it is ours, so it is published without a second upload.
func TestLostUploadReplyThroughS3(t *testing.T) {
	r := newS3Rig(t)
	res, err := r.h.GetAt(ctx, "t/a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.DB.Query(ctx, vactortest.Put("v1")); err != nil {
		t.Fatal(err)
	}
	r.fake.ResetRequests()
	r.fake.Inject(s3test.Fault{Mode: s3test.ApplyThenDropReply, Method: "PUT", Times: 1})
	wb, err := r.h.WriteBack(ctx, "t/a")
	if err != nil || wb.Generation != 1 {
		t.Fatalf("WriteBack = %+v, %v", wb, err)
	}
	genPuts := 0
	for _, q := range r.fake.Requests() {
		if q.Method == "PUT" && strings.Contains(q.Path, "/gen/") {
			genPuts++
		}
	}
	if genPuts != 1 {
		t.Fatalf("%d generation PUTs, want 1", genPuts)
	}
	if m, _, err := r.d.ReadManifest(ctx, "t/a"); err != nil || m.Generation != 1 {
		t.Fatalf("manifest %+v, %v", m, err)
	}
}

// OS-06 "bucket bytes flat": through the real adapter, 30 write-backs of a 64 KiB database leave the key's
// prefix at (K+1) × 64 KiB plus the manifest after every one. Against the in-process S3 always, and against
// SeaweedFS when COLDLOAD_S3 is set.
func TestBucketBytesFlatThroughS3(t *testing.T) {
	run := func(t *testing.T, d *objstore.Durable) {
		h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: d, CreateMissing: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		const size, k = 64 << 10, 2
		for i := range 30 {
			res, err := h.Get(ctx, "t/flat")
			if err != nil {
				t.Fatal(err)
			}
			v := strings.Repeat(string(rune('a'+i%26)), size)
			if err := res.DB.Query(ctx, vactortest.Put(v)); err != nil {
				t.Fatal(err)
			}
			if _, err := h.WriteBack(ctx, "t/flat"); err != nil {
				t.Fatal(err)
			}
			l, err := d.ListKey(ctx, "t/flat")
			if err != nil {
				t.Fatal(err)
			}
			if gens := len(l.Generations); gens > k+1 || l.Bytes()-l.Manifest.Size > (k+1)*size {
				t.Fatalf("after write-back %d: %d generations, %d bytes; want ≤ %d and ≤ %d", i, gens, l.Bytes(), k+1, (k+1)*size)
			}
		}
		if st := h.Stats(); st.GenerationsDeleted != 27 {
			t.Fatalf("stats %+v, want 27 generations deleted", st)
		}
	}
	t.Run("in-process S3", func(t *testing.T) { run(t, newS3Rig(t).d) })
	t.Run("SeaweedFS", func(t *testing.T) {
		ep := os.Getenv("COLDLOAD_S3")
		if ep == "" {
			t.Skip("set COLDLOAD_S3=host:port to run against a real S3")
		}
		st, err := objstore.New(objstore.Config{Endpoint: ep, AccessKey: "coldload", SecretKey: "coldload-secret", Bucket: "coldload"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.EnsureBucket(ctx); err != nil {
			t.Fatal(err)
		}
		root := "test-" + logx.NewID() + "/"
		t.Cleanup(func() { st.DeletePrefix(context.Background(), root) })
		run(t, objstore.NewDurable(st, root))
	})
}

// OS-10 measurement against SeaweedFS (COLDLOAD_S3): the cold activation of a 256 MiB generation (manifest
// GET + download) against a re-warm from the generation cache (manifest GET + rename). Logs both; fails only
// if the re-warm downloads or takes over 100 ms.
func TestGenCacheRewarmLatencyS3(t *testing.T) {
	ep := os.Getenv("COLDLOAD_S3")
	if ep == "" {
		t.Skip("set COLDLOAD_S3=host:port to run against a real S3")
	}
	const size = 256 << 20
	st, err := objstore.New(objstore.Config{Endpoint: ep, AccessKey: "coldload", SecretKey: "coldload-secret", Bucket: "coldload"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	root := "test-" + logx.NewID() + "/"
	t.Cleanup(func() { st.DeletePrefix(context.Background(), root) })
	d := objstore.NewDurable(st, root)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	src := filepath.Join(t.TempDir(), "src.db")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	m := durable.NextManifest("t/big", 1, 0, durabletest.DigestOf(data), time.Now())
	data = nil
	if err := d.PutGeneration(ctx, m, src); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PublishManifest(ctx, m, durable.NoVersion); err != nil {
		t.Fatal(err)
	}
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: d,
		GenCacheBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	for i := range 3 {
		res, err := h.Get(ctx, "t/big")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("activation %d: local hit %v, total %v (fetch %v)", i, res.Act.LocalHit, res.Act.Total, res.Act.Fetch)
		if i > 0 && (!res.Act.LocalHit || res.Act.Total > 100*time.Millisecond) {
			t.Fatalf("re-warm %d: %+v; want a local hit under 100 ms", i, res.Act)
		}
		t0 := time.Now()
		if err := h.Passivate(ctx, "t/big"); err != nil {
			t.Fatal(err)
		}
		t.Logf("passivation %d (clean: hash + rename into the cache): %v", i, time.Since(t0))
	}
}
