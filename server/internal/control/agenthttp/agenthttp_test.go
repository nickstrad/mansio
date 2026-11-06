package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/owner"
)

var ctx = context.Background()

// fakeAgent answers with status and body, and records the last request.
func fakeAgent(t *testing.T, status int, body any) (*httptest.Server, *atomic.Value) {
	var last atomic.Value
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(r.Clone(context.Background()))
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.Close)
	return s, &last
}

func client(t *testing.T, url string, mod func(*Config)) *Client {
	cfg := Config{VM: "vm-a", BaseURL: url, Token: "tok", Timeout: time.Second}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAnswersMapToSentinels(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{409, "wrong_owner", owner.ErrWrongOwner},
		{409, "moving", owner.ErrMoving},
		{409, "fenced", owner.ErrFenced},
		{409, "illegal", owner.ErrIllegal},
		{400, "bad_request", owner.ErrInvalid},
		{401, "unauthorized", ErrUnauthorized},
		{503, "unavailable", ErrUnavailable},
		{504, "timeout", ErrUnavailable},
		{500, "internal", ErrUnavailable},
		{418, "teapot", ErrUnavailable},
	} {
		s, _ := fakeAgent(t, tc.status, map[string]string{"code": tc.code, "message": "m"})
		_, err := client(t, s.URL, nil).Fence(ctx, "k")
		if !errors.Is(err, tc.want) {
			t.Errorf("%d %s: %v, want %v", tc.status, tc.code, err, tc.want)
		}
	}
}

func TestOKAnswersAndRequest(t *testing.T) {
	s, last := fakeAgent(t, 200, map[string]any{"code": "ok", "db": "k", "state": "active", "epoch": 3, "gen": 7})
	c := client(t, s.URL, nil)
	l, err := c.Claim(ctx, "k")
	if err != nil || l != (owner.Local{State: owner.Active, Epoch: 3, Gen: 7}) {
		t.Fatalf("Claim = %+v, %v", l, err)
	}
	r := last.Load().(*http.Request)
	ms, _ := strconv.Atoi(r.Header.Get("X-Timeout-Ms"))
	if r.URL.Path != claimPath || r.Header.Get("Authorization") != "Bearer tok" || ms <= 0 || ms > 1000 {
		t.Fatalf("request %s %v", r.URL.Path, r.Header)
	}
	ack, err := c.Fence(ctx, "k")
	if err != nil || ack != (owner.Ack{Key: "k", Epoch: 3, Gen: 7}) {
		t.Fatalf("Fence = %+v, %v", ack, err)
	}
}

// Transport failures trip this agent's breaker; then calls fail fast without sending; 409 answers never trip it.
func TestBreaker(t *testing.T) {
	s, _ := fakeAgent(t, 409, map[string]string{"code": "wrong_owner"})
	c := client(t, s.URL, func(c *Config) { c.Breaker = breaker.Config{MinCalls: 2, CoolDown: time.Minute} })
	for range 5 {
		if _, err := c.Fence(ctx, "k"); !errors.Is(err, owner.ErrWrongOwner) {
			t.Fatal(err)
		}
	}
	dead := client(t, "http://127.0.0.1:1", func(c *Config) { c.Breaker = breaker.Config{MinCalls: 2, CoolDown: time.Minute} })
	for range 2 {
		if _, err := dead.Fence(ctx, "k"); !errors.Is(err, ErrUnavailable) || errors.Is(err, breaker.ErrOpen) {
			t.Fatalf("refused connection: %v", err)
		}
	}
	if _, err := dead.Fence(ctx, "k"); !errors.Is(err, breaker.ErrOpen) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("after 2 failures: %v, want the breaker open", err)
	}
}

// A slow agent is cut off at the call's timeout.
func TestDeadline(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(s.Close)
	c := client(t, s.URL, func(c *Config) { c.Timeout = 100 * time.Millisecond })
	t0 := time.Now()
	if _, err := c.Health(ctx); !errors.Is(err, ErrUnavailable) || time.Since(t0) > time.Second {
		t.Fatalf("slow agent: %v after %v", err, time.Since(t0))
	}
}
