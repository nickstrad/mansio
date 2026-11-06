package edge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
)

// registrySrv is controld's GET /v1/agents with a fault switch: body, when set, is answered as is.
type registrySrv struct {
	mu     sync.Mutex
	status int
	body   string
	gets   int
}

func (s *registrySrv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(s.status)
	w.Write([]byte(s.body))
}

func (s *registrySrv) set(status int, body string) {
	s.mu.Lock()
	s.status, s.body = status, body
	s.mu.Unlock()
}

func TestRegistryRoutesKeepTheLastGoodMap(t *testing.T) {
	srv := &registrySrv{status: 200, body: `{"code":"ok","agents":{"vm-a":"10.0.0.1:7070"}}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	r, err := NewRegistryRoutes(RegistryConfig{URL: ts.URL, Token: "tok", Every: time.Hour, Timeout: time.Second,
		Breaker: breaker.Config{MinCalls: 3, CoolDown: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Agent("vm-a"); ok {
		t.Fatal("routes before the first fetch")
	}
	if err := r.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u, ok := r.Agent("vm-a"); !ok || u != "https://10.0.0.1:7070" {
		t.Fatalf("vm-a: %q %v (want the default https scheme)", u, ok)
	}
	for _, bad := range []struct {
		status int
		body   string
	}{{500, ""}, {200, "not json"}, {200, `{"agents":{"":"x:1"}}`}, {200, `{"agents":{"vm-b":""}}`}} {
		srv.set(bad.status, bad.body)
		if err := r.Fetch(context.Background()); err == nil {
			t.Fatalf("%d %q accepted", bad.status, bad.body)
		}
		if u, ok := r.Agent("vm-a"); !ok || u != "https://10.0.0.1:7070" || r.Len() != 1 {
			t.Fatalf("after %d %q the last map is gone: %q %v", bad.status, bad.body, u, ok)
		}
	}
	// Three failures in the window opened the breaker: the next fetch makes no call.
	srv.mu.Lock()
	before := srv.gets
	srv.mu.Unlock()
	if err := r.Fetch(context.Background()); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("breaker: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.gets != before {
		t.Fatal("a fetch went out through an open breaker")
	}
}

// An unknown VM asks for an early fetch: Run fetches then, not a period later, and coalesces the asks.
func TestRegistryRoutesFetchEarlyForAnUnknownVM(t *testing.T) {
	srv := &registrySrv{status: 200, body: `{"agents":{"vm-a":"a:1"}}`}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	r, err := NewRegistryRoutes(RegistryConfig{URL: ts.URL, Token: "tok", Scheme: "http", Every: time.Hour, Timeout: time.Second,
		MinGap: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	srv.set(200, `{"agents":{"vm-a":"a:1","vm-b":"b:1"}}`)
	for range 5 {
		r.Agent("vm-b")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if u, ok := r.Agent("vm-b"); ok && u == "http://b:1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("vm-b never fetched")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := NewRegistryRoutes(RegistryConfig{URL: ts.URL, Every: time.Second, Timeout: time.Second}); err == nil {
		t.Fatal("timeout = every accepted")
	}
}
