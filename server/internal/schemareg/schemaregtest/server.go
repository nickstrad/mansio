// Package schemaregtest is a fake schema registry: an httptest server speaking controld's
// GET /v1/apps/{app}/schemas/{fingerprint}, with fault switches (Release It H4), for schemareg.Client's tests
// and for anything built on it (IR-2b's executor).
package schemaregtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// Token is the bearer token the fake accepts.
const Token = "test-control-token-0123456789"

// Server is a fake registry. Safe for concurrent use.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	schemas map[[2]string]string // (app, fingerprint) → text
	status  int                  // answer every request with this status (0 = behave)
	delay   time.Duration        // before answering
	corrupt bool                 // answer a text that does not match its fingerprint
	block   chan struct{}        // while non-nil, requests wait until it is closed
	calls   atomic.Int64
}

// New starts a fake registry, closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{schemas: map[[2]string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/apps/{app}/schemas/{fingerprint}", s.get)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Add registers text for app and returns its fingerprint.
func (s *Server) Add(app, text string) string {
	fp := schemareg.Fingerprint([]byte(text))
	s.mu.Lock()
	s.schemas[[2]string{app, fp}] = text
	s.mu.Unlock()
	return fp
}

// Fail makes every request answer status (0 restores normal answers).
func (s *Server) Fail(status int) { s.mu.Lock(); s.status = status; s.mu.Unlock() }

// Delay makes every request wait d before answering.
func (s *Server) Delay(d time.Duration) { s.mu.Lock(); s.delay = d; s.mu.Unlock() }

// Corrupt makes answers carry a text that does not hash to the fingerprint asked for.
func (s *Server) Corrupt(on bool) { s.mu.Lock(); s.corrupt = on; s.mu.Unlock() }

// Block holds every request until the returned func is called.
func (s *Server) Block() (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.block = ch
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.block = nil
			s.mu.Unlock()
			close(ch)
		})
	}
}

// Calls is how many requests arrived.
func (s *Server) Calls() int64 { return s.calls.Load() }

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	s.mu.Lock()
	status, delay, corrupt, block := s.status, s.delay, s.corrupt, s.block
	app, fp := r.PathValue("app"), r.PathValue("fingerprint")
	text, ok := s.schemas[[2]string{app, fp}]
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Header.Get("Authorization") != "Bearer "+Token:
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized"})
	case status != 0:
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"code": "injected"})
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"code": "not_found"})
	default:
		if corrupt {
			text += " "
		}
		json.NewEncoder(w).Encode(map[string]string{"code": "ok", "app": app, "fingerprint": fp, "schema": text})
	}
}
