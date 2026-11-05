package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/edge/edgetest"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
)

// With a tls section the query listener speaks HTTPS (h2 by ALPN) with a private CA's certificate; plain HTTP
// gets nothing useful.
func TestQueryListenerTLS(t *testing.T) {
	dir := t.TempDir()
	ca := edgetest.NewCA(t)
	_, certFile, keyFile := ca.WriteFiles(t, dir, "agent", "127.0.0.1")
	cfg := fixture(t, []string{"db-1"}, fmt.Sprintf(`,"tls":{"cert_file":%q,"key_file":%q}`, certFile, keyFile))
	addrFile := filepath.Join(t.TempDir(), "addr")
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuf
	done := make(chan int)
	go func() { done <- realMain(ctx, []string{"-config", cfg, "-addr-file", addrFile}, &stderr) }()
	addr := waitAddr(t, addrFile, done)

	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}, ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	b, _ := json.Marshal(map[string]any{"db": "db-1", "kind": "read", "sql": fronttest.GetSQL, "args": []any{2}})
	req, _ := http.NewRequest(http.MethodPost, "https://"+addr+front.QueryPath, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+iss.Mint(t, iss.Claims("db-1", "vm-1", time.Now(), time.Minute)))
	var out front.Response
	for range 100 { // the database opens in the background
		res, err := client.Do(req.Clone(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		if res.ProtoMajor != 2 {
			t.Fatalf("proto %s, want HTTP/2 over TLS", res.Proto)
		}
		out = front.Response{}
		json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if out.Code == front.OK {
			break
		}
		req.Body, _ = req.GetBody()
		time.Sleep(10 * time.Millisecond)
	}
	if out.Code != front.OK || out.Count != 1 {
		t.Fatalf("over TLS: %+v", out)
	}
	if res, err := http.Post("http://"+addr+front.QueryPath, "application/json", nil); err == nil {
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("plain HTTP to the TLS listener: %d", res.StatusCode)
		}
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
}

// A query listener off loopback needs tls, or -dev.
func TestNonLoopbackListenNeedsTLSOrDev(t *testing.T) {
	cfg := fixture(t, []string{"db-1"}, `,"listen":"0.0.0.0:0"`)
	var stderr syncBuf
	if code := realMain(context.Background(), []string{"-config", cfg, "-check"}, &stderr); code != exitError ||
		!strings.Contains(stderr.String(), "not loopback and there is no tls section") {
		t.Fatalf("without -dev: exit %d\n%s", code, stderr.String())
	}
	stderr = syncBuf{}
	if code := realMain(context.Background(), []string{"-config", cfg, "-check", "-dev"}, &stderr); code != exitOK {
		t.Fatalf("with -dev: exit %d\n%s", code, stderr.String())
	}
	bad := fixture(t, []string{"db-1"}, `,"tls":{"cert_file":"missing.crt","key_file":"missing.key"}`)
	stderr = syncBuf{}
	if code := realMain(context.Background(), []string{"-config", bad, "-check"}, &stderr); code != exitError ||
		!strings.Contains(stderr.String(), "cannot load the tls certificate") {
		t.Fatalf("missing cert: exit %d\n%s", code, stderr.String())
	}
}
