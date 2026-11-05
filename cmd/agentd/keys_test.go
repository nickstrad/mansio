package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/ticket"
	"github.com/nickstrad/ts-db-go/internal/ticket/tickettest"
)

// keyServer is a fake issuer's GET /v1/keys with a fault switch.
type keyServer struct {
	mu     sync.Mutex
	keys   []tickettest.Issuer
	status int // non-zero: answer this instead
	gets   int
}

func (s *keyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	f := ticket.PublicKeyFile{}
	for _, k := range s.keys {
		f.Keys = append(f.Keys, ticket.PublicKeyEntry{KID: k.KID, PublicKey: ticket.EncodeKey(k.Pub)})
	}
	json.NewEncoder(w).Encode(f)
}

func (s *keyServer) set(status int, keys ...tickettest.Issuer) {
	s.mu.Lock()
	s.status, s.keys = status, keys
	s.mu.Unlock()
}

func kids(r *ticket.KeyRing) []string { return slices.Sorted(slices.Values(r.KIDs())) }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestKeyFetcher(t *testing.T) {
	k1, k2 := tickettest.NewIssuer("k1"), tickettest.NewIssuer("k2")
	ks := &keyServer{keys: []tickettest.Issuer{k1}}
	srv := httptest.NewServer(ks)
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "keys.json")
	cfg := &Config{KeysURL: srv.URL + "/v1/keys", KeysFile: cache}
	cfg.KeysRefresh.Duration, cfg.KeysTimeout.Duration = 20*time.Millisecond, time.Second

	ring, kf, err := openKeys(context.Background(), cfg, quietLog())
	if err != nil || !slices.Equal(kids(ring), []string{"k1"}) {
		t.Fatalf("first fetch: %v %v", kids(ring), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { kf.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Rotation: the issuer publishes k2 next to k1, then drops k1.
	ks.set(0, k1, k2)
	waitUntil(t, func() bool { return slices.Equal(kids(ring), []string{"k1", "k2"}) })
	ks.set(0, k2)
	waitUntil(t, func() bool { return slices.Equal(kids(ring), []string{"k2"}) })

	// The issuer fails: the ring stays, failures are counted.
	ks.set(http.StatusServiceUnavailable)
	waitUntil(t, func() bool { return kf.failures.Load() >= 2 })
	if !slices.Equal(kids(ring), []string{"k2"}) {
		t.Fatalf("a failed fetch changed the ring: %v", kids(ring))
	}
	ks.set(0, k2)
	waitUntil(t, func() bool { return !kf.failing.Load() })

	// A restart while the issuer is down: the cached set verifies tickets.
	srv.Close()
	ring2, _, err := openKeys(context.Background(), cfg, quietLog())
	if err != nil || !slices.Equal(kids(ring2), []string{"k2"}) {
		t.Fatalf("restart from the cache: %v %v", kids(ring2), err)
	}
	// No cache and no issuer: start with no keys (every ticket is refused until a fetch succeeds), not fail.
	cfg.KeysFile = ""
	ring3, _, err := openKeys(context.Background(), cfg, quietLog())
	if err != nil || len(ring3.KIDs()) != 0 {
		t.Fatalf("no keys anywhere: %v %v", kids(ring3), err)
	}
}

func TestKeysConfig(t *testing.T) {
	base := `"vm":"vm-a","db_dir":"d"`
	for _, tc := range []struct{ name, body, problem string }{
		{"neither", `{` + base + `}`, "keys_file or keys_url"},
		{"url only", `{` + base + `,"keys_url":"https://c/v1/keys"}`, ""},
		{"not a url", `{` + base + `,"keys_url":"c:7080"}`, "http(s) URL"},
		{"timeout = refresh (off point)", `{` + base + `,"keys_url":"https://c/v1/keys","keys_refresh":"3s","keys_timeout":"3s"}`, "keys_timeout shorter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig(strings.NewReader(tc.body), "/etc")
			if (tc.problem == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("got %v, want %q", err, tc.problem)
			}
		})
	}
	for _, tc := range []struct {
		url string
		dev bool
		ok  bool
	}{{"http://c/v1/keys", false, false}, {"http://c/v1/keys", true, true}, {"https://c/v1/keys", false, true}} {
		c := &Config{KeysURL: tc.url, Dev: tc.dev}
		if err := c.checkDev(); (err == nil) != tc.ok {
			t.Errorf("%s dev %v: %v", tc.url, tc.dev, err)
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
