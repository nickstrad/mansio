package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

// Integration tests against a real S3 endpoint. Skipped unless COLDLOAD_S3 is set, e.g.
//
//	docker compose -f deploy/seaweedfs/docker-compose.yml up -d
//	COLDLOAD_S3=localhost:8333 go test ./cmd/coldload -run S3
func s3Store(t *testing.T) *objstore.Store {
	t.Helper()
	ep := os.Getenv("COLDLOAD_S3")
	if ep == "" {
		t.Skip("set COLDLOAD_S3=host:port to run S3 integration tests")
	}
	st, err := objstore.New(objstore.Config{Endpoint: ep, AccessKey: "coldload", SecretKey: "coldload-secret", Bucket: "coldload"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// The real adapter must pass the same contract as the fake the unit tests use.
func TestS3FetcherContract(t *testing.T) {
	st := s3Store(t)
	vactortest.RunFetcherSuite(t, func(t *testing.T) (vactor.Fetcher, func(string, []byte)) {
		prefix := "test-" + logx.NewID() + "/"
		t.Cleanup(func() { st.DeletePrefix(context.Background(), prefix) })
		put := func(key string, data []byte) {
			p := filepath.Join(t.TempDir(), "obj")
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := st.PutFile(context.Background(), prefix+key, p); err != nil {
				t.Fatal(err)
			}
		}
		return prefixed{s3Fetcher{st}, prefix}, put
	})
}

type prefixed struct {
	f      vactor.Fetcher
	prefix string
}

func (p prefixed) Fetch(ctx context.Context, key, path string) (vactor.Fetched, error) {
	return p.f.Fetch(ctx, p.prefix+key, path)
}

func TestS3StoreDownIsWrapped(t *testing.T) {
	st, err := objstore.New(objstore.Config{Endpoint: "127.0.0.1:1", AccessKey: "a", SecretKey: "b", Bucket: "c"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.GetToFile(context.Background(), "k", filepath.Join(t.TempDir(), "x"))
	if e, ok := errs.Outermost(err); !ok || e.Module != "objstore" || errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("err = %v (%T); want an objstore error that is not ErrNotFound", err, err)
	}
}

func TestObjstoreNewValidates(t *testing.T) {
	if _, err := objstore.New(objstore.Config{Endpoint: "x"}); err == nil {
		t.Fatal("New accepted a config without credentials and bucket")
	}
}
