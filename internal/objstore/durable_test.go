package objstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/objstore"
)

// The S3 adapter must pass the same contract as the fake (including the concurrent-publish test, which is
// what checks that the server honours If-Match / If-None-Match). Skipped unless COLDLOAD_S3 is set:
//
//	docker compose -f deploy/seaweedfs/docker-compose.yml up -d
//	COLDLOAD_S3=localhost:8333 go test ./internal/objstore -run S3
func TestS3DurableStoreContract(t *testing.T) {
	ep := os.Getenv("COLDLOAD_S3")
	if ep == "" {
		t.Skip("set COLDLOAD_S3=host:port to run the S3 durable store contract suite")
	}
	st, err := objstore.New(objstore.Config{Endpoint: ep, AccessKey: "coldload", SecretKey: "coldload-secret", Bucket: "coldload"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	durabletest.RunStoreSuite(t, func(t *testing.T) durable.Store {
		root := "test-" + logx.NewID() + "/"
		t.Cleanup(func() { st.DeletePrefix(context.Background(), root) })
		return objstore.NewDurable(st, root)
	})
}

// Runs without a store: every call against an unreachable endpoint is a wrapped objstore error, and never
// one of the sentinels that would make the host decide something (no manifest, stale, exists).
func TestDurableStoreDownIsAWrappedErrorNotASentinel(t *testing.T) {
	st, err := objstore.New(objstore.Config{Endpoint: "127.0.0.1:1", AccessKey: "a", SecretKey: "b", Bucket: "c"})
	if err != nil {
		t.Fatal(err)
	}
	d := objstore.NewDurable(st, "")
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("x"), 0o644)
	m := durable.NextManifest("k", 1, 0, durabletest.DigestOf([]byte("x")), time.Now())
	_, _, e1 := d.ReadManifest(c, "k")
	e2 := d.PutGeneration(c, "k", 1, path, durabletest.DigestOf([]byte("x")))
	_, e3 := d.FetchGeneration(c, m, filepath.Join(t.TempDir(), "out"))
	_, e4 := d.PublishManifest(c, m, durable.NoVersion)
	for i, err := range []error{e1, e2, e3, e4} {
		if e, ok := errs.Outermost(err); !ok || e.Module != "objstore" {
			t.Errorf("call %d: %v is not a wrapped objstore error", i, err)
		}
		for _, s := range []error{durable.ErrNoManifest, durable.ErrStale, durable.ErrExists, durable.ErrNotFound} {
			if errors.Is(err, s) {
				t.Errorf("call %d: store down matched %v", i, s)
			}
		}
	}
	big := durable.Digest{Size: 6 << 30, SHA256: m.SHA256}
	if err := d.PutGeneration(c, "k", 2, path, big); err == nil {
		t.Error("a 6 GiB single PUT was attempted")
	}
}

// The adapter's conditional logic against an in-process S3 that honours the headers exactly. This checks
// what the adapter sends and how it maps the answers; whether SeaweedFS honours them is
// TestS3DurableStoreContract's job.
func TestDurableStoreContractAgainstFakeS3(t *testing.T) {
	durabletest.RunStoreSuite(t, func(t *testing.T) durable.Store {
		fake, ep := newFakeS3(t)
		st, err := objstore.New(objstore.Config{Endpoint: ep, AccessKey: "a", SecretKey: "b", Bucket: "bkt"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			for _, h := range fake.puts {
				if h.Get("If-None-Match") == "" && h.Get("If-Match") == "" {
					t.Errorf("an unconditional PUT was sent: %v", h)
				}
				if h.Get("Content-Type") == "application/vnd.sqlite3" && h.Get("X-Amz-Meta-Sha256") == "" {
					t.Errorf("a generation PUT without x-amz-meta-sha256: %v", h)
				}
			}
		})
		return objstore.NewDurable(st, "root/")
	})
}
