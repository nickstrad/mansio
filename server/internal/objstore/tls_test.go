package objstore_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/objstore/s3test"
)

// OS-11: Secure and CAFile are validated at the edge (New), before any request.
func TestTLSConfigValidation(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	os.WriteFile(garbage, []byte("not a certificate"), 0o644)
	srv := s3test.NewTLS()
	t.Cleanup(srv.Close)
	good := filepath.Join(dir, "ca.pem")
	os.WriteFile(good, srv.CAPEM(), 0o644)
	for _, tc := range []struct {
		name    string
		secure  bool
		ca      string
		problem string // "" = accepted
	}{
		{"plain", false, "", ""},
		{"secure, system roots", true, "", ""},
		{"secure, private CA", true, good, ""},
		{"CA file without secure", false, good, "Secure is off"},
		{"missing CA file", true, filepath.Join(dir, "none.pem"), "cannot read the CA file"},
		{"CA file without a certificate", true, garbage, "holds no PEM certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := objstore.New(objstore.Config{Endpoint: "e:1", AccessKey: "a", SecretKey: "b", Bucket: "bkt", Secure: tc.secure, CAFile: tc.ca})
			if tc.problem == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("err %v, want %q", err, tc.problem)
			}
		})
	}
}

// OS-11: over HTTPS the client reaches a server signed by the configured CA, and refuses one it cannot verify
// (no CA file: the self-signed certificate is unknown) or a plain-HTTP client talking to a TLS port. Only the
// trusted client's request reaches the handler.
func TestTLSReachesOnlyTrustedServer(t *testing.T) {
	srv := s3test.NewTLS()
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, srv.CAPEM(), 0o644)
	open := func(secure bool, caFile string) *objstore.Store {
		st, err := objstore.New(objstore.Config{Endpoint: srv.Endpoint(), AccessKey: "a", SecretKey: "b", Bucket: "bkt",
			Secure: secure, CAFile: caFile, OpTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := open(true, ca).EnsureBucket(ctx); err != nil {
		t.Fatalf("trusted CA: %v", err)
	}
	if srv.Count("HEAD") != 1 {
		t.Fatalf("bucket not checked over TLS: %d HEADs", srv.Count("HEAD"))
	}
	// An unverifiable certificate stays Transient (retried, trips the breaker): from the agent's side an
	// expired or rotated server certificate is a dependency outage, answered unavailable, never internal.
	if err := open(true, "").EnsureBucket(ctx); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("unknown CA: %v, want a certificate failure", err)
	}
	if err := open(false, "").EnsureBucket(ctx); err == nil {
		t.Fatal("plain HTTP to a TLS port succeeded")
	}
	if n := srv.Count("HEAD"); n != 1 {
		t.Fatalf("%d HEADs reached the server, want only the trusted client's", n)
	}
}
