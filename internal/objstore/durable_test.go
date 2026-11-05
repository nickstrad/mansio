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
	"github.com/nickstrad/ts-db-go/internal/faultnet"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/objstore/s3test"
)

// The S3 adapter must pass the same contract as the fake (including the concurrent-publish test, which is
// what checks that the server honours If-Match / If-None-Match). Skipped unless COLDLOAD_S3 is set:
//
//	docker compose -f deploy/seaweedfs/docker-compose.yml up -d
//	COLDLOAD_S3=localhost:8333 go test ./internal/objstore -run S3
func TestS3DurableStoreContract(t *testing.T) {
	ep := s3Endpoint(t)
	t.Run("direct", func(t *testing.T) { runS3Suite(t, ep) })
	t.Run("through healthy faultnet proxy", func(t *testing.T) {
		p, err := faultnet.New(ep)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		runS3Suite(t, p.Addr())
	})
}

// s3Endpoint returns COLDLOAD_S3 or skips the test.
func s3Endpoint(t *testing.T) string {
	t.Helper()
	ep := os.Getenv("COLDLOAD_S3")
	if ep == "" {
		t.Skip("set COLDLOAD_S3=host:port to run tests against a real S3")
	}
	return ep
}

// s3Config is the real S3 at ep with the coldload credentials (deploy/seaweedfs/s3.json).
func s3Config(ep string) objstore.Config {
	return objstore.Config{Endpoint: ep, AccessKey: "coldload", SecretKey: "coldload-secret", Bucket: "coldload"}
}

// s3Store connects to the real S3 at ep and makes sure the bucket exists.
func s3Store(t *testing.T, ep string) *objstore.Store {
	t.Helper()
	st := mustStore(t, s3Config(ep))
	if err := st.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func runS3Suite(t *testing.T, ep string) {
	st := s3Store(t, ep)
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
		fake := s3test.New()
		t.Cleanup(fake.Close)
		t.Cleanup(func() {
			for _, r := range fake.Requests() {
				if r.Method != "PUT" {
					continue
				}
				if r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Match") == "" {
					t.Errorf("an unconditional PUT was sent: %v", r.Header)
				}
				if r.Header.Get("Content-Md5") == "" {
					t.Errorf("a PUT without Content-MD5 (OS-07): %v", r.Header)
				}
				if r.Header.Get("Content-Type") == "application/vnd.sqlite3" && r.Header.Get("X-Amz-Meta-Sha256") == "" {
					t.Errorf("a generation PUT without x-amz-meta-sha256: %v", r.Header)
				}
			}
		})
		return objstore.NewDurable(mustStore(t, fakeConfig(fake.Endpoint())), "root/")
	})
}

// The same suite through a faultnet proxy with every fault off: the harness itself must be transparent
// before any fault test through it means something (RI-13).
func TestDurableStoreContractThroughHealthyProxy(t *testing.T) {
	durabletest.RunStoreSuite(t, func(t *testing.T) durable.Store {
		fake := s3test.New()
		t.Cleanup(fake.Close)
		p, err := faultnet.New(fake.Endpoint())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		return objstore.NewDurable(mustStore(t, fakeConfig(p.Addr())), "root/")
	})
}

// fakeConfig is a store at endpoint with the credentials and bucket the s3test fake accepts.
func fakeConfig(endpoint string) objstore.Config {
	return objstore.Config{Endpoint: endpoint, AccessKey: "a", SecretKey: "b", Bucket: "bkt"}
}

func mustStore(t *testing.T, cfg objstore.Config) *objstore.Store {
	t.Helper()
	st, err := objstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// srcFile writes data to a temp file and returns its path and digest.
func srcFile(t *testing.T, data []byte) (string, durable.Digest) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, durabletest.DigestOf(data)
}
