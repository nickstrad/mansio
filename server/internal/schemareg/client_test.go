package schemareg_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/schemareg"
	"github.com/nickstrad/mansio/server/internal/schemareg/schemaregtest"
)

const text = `{"games":{"fields":{"x":{"node":{"kind":"string"}}},"storage":"columns","indexes":[{"name":"by_x","fields":["x"]}]}}`

func client(t *testing.T, s *schemaregtest.Server, mod func(*schemareg.Config)) *schemareg.Client {
	t.Helper()
	cfg := schemareg.Config{URL: s.URL, Token: schemaregtest.Token, Timeout: time.Second,
		Breaker: breaker.Config{MinCalls: 3, FailureRate: 0.5, Window: time.Minute, CoolDown: time.Minute}}
	if mod != nil {
		mod(&cfg)
	}
	c, err := schemareg.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGetFetchesOnceThenServesTheCache(t *testing.T) {
	s := schemaregtest.New(t)
	fp := s.Add("tic-tac-toe", text)
	c := client(t, s, nil)
	for range 3 {
		got, err := c.Get(context.Background(), "tic-tac-toe", fp)
		if err != nil || got != text {
			t.Fatalf("Get = %q, %v", got, err)
		}
	}
	if st := c.Stats(); s.Calls() != 1 || st.Hits != 2 || st.Loads != 1 || st.Entries != 1 || st.Bytes != int64(len(text)) {
		t.Fatalf("calls %d, stats %+v", s.Calls(), st)
	}
}

func TestGetIsScopedToTheApp(t *testing.T) {
	s := schemaregtest.New(t)
	fp := s.Add("a", text)
	c := client(t, s, nil)
	if _, err := c.Get(context.Background(), "b", fp); !errors.Is(err, schemareg.ErrNotFound) {
		t.Fatalf("another app's fingerprint: %v", err)
	}
}

// Not found is an answer: not cached (a registration may follow), and never counted against the breaker.
func TestNotFoundIsAnAnswer(t *testing.T) {
	s := schemaregtest.New(t)
	c := client(t, s, nil)
	fp := schemareg.Fingerprint([]byte(text))
	for range 5 {
		if _, err := c.Get(context.Background(), "app", fp); !errors.Is(err, schemareg.ErrNotFound) {
			t.Fatalf("Get = %v", err)
		}
	}
	if c.Breaker().State(time.Now()) != breaker.Closed || s.Calls() != 5 || c.Stats().Failures != 0 {
		t.Fatalf("breaker %v, calls %d, stats %+v", c.Breaker().State(time.Now()), s.Calls(), c.Stats())
	}
	s.Add("app", text)
	if got, err := c.Get(context.Background(), "app", fp); err != nil || got != text {
		t.Fatalf("after registering: %q, %v", got, err)
	}
}

func TestMalformedInputSendsNothing(t *testing.T) {
	s := schemaregtest.New(t)
	c := client(t, s, nil)
	for _, tc := range [][2]string{{"", schemareg.Fingerprint([]byte("{}"))}, {"app", "sha256:nope"}} {
		if _, err := c.Get(context.Background(), tc[0], tc[1]); !errors.Is(err, schemareg.ErrInvalid) {
			t.Fatalf("%v: %v", tc, err)
		}
	}
	if s.Calls() != 0 {
		t.Fatalf("sent %d requests", s.Calls())
	}
}

// The fault table (Release It H4): each system failure is ErrUnavailable, counts against the breaker, and is
// never cached; once the breaker opens, nothing is sent and the error carries a retry hint.
func TestFaultsAreUnavailableAndOpenTheBreaker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(s *schemaregtest.Server)
	}{
		{"5xx", func(s *schemaregtest.Server) { s.Fail(http.StatusServiceUnavailable) }},
		{"401 (wrong token)", func(s *schemaregtest.Server) { s.Fail(http.StatusUnauthorized) }},
		{"timeout", func(s *schemaregtest.Server) { s.Delay(time.Second) }},
		{"corrupt text", func(s *schemaregtest.Server) { s.Corrupt(true) }},
		{"connection refused", func(s *schemaregtest.Server) { s.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := schemaregtest.New(t)
			fp := s.Add("app", text)
			c := client(t, s, func(cfg *schemareg.Config) { cfg.Timeout = 50 * time.Millisecond })
			tc.inject(s)
			for range 3 {
				if _, err := c.Get(context.Background(), "app", fp); !errors.Is(err, schemareg.ErrUnavailable) {
					t.Fatalf("Get = %v", err)
				}
			}
			calls := s.Calls()
			_, err := c.Get(context.Background(), "app", fp)
			var hint interface{ RetryAfter() time.Duration }
			if !errors.Is(err, schemareg.ErrUnavailable) || !errors.As(err, &hint) || hint.RetryAfter() <= 0 || s.Calls() != calls {
				t.Fatalf("open breaker: %v (calls %d → %d)", err, calls, s.Calls())
			}
			if st := c.Stats(); st.Failures != 3 || st.Shed != 1 || st.Entries != 0 {
				t.Fatalf("stats %+v", st)
			}
		})
	}
}

// The Validate hook (IR-2b's docschema check) refuses a text before it is cached.
func TestValidateHook(t *testing.T) {
	s := schemaregtest.New(t)
	fp := s.Add("app", text)
	bad := errors.New("not a schema")
	c := client(t, s, func(cfg *schemareg.Config) {
		cfg.Validate = func(string, []byte) error { return bad }
	})
	if _, err := c.Get(context.Background(), "app", fp); !errors.Is(err, schemareg.ErrUnavailable) || !errors.Is(err, bad) {
		t.Fatalf("Get = %v", err)
	}
	if c.Stats().Entries != 0 {
		t.Fatal("a refused text was cached")
	}
}

// Concurrent callers of one key share one fetch; a caller that gives up does not cancel it for the others.
func TestSingleFlightAndDetachedFetch(t *testing.T) {
	s := schemaregtest.New(t)
	fp := s.Add("app", text)
	c := client(t, s, nil)
	release := s.Block()
	leaver, cancel := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() { _, err := c.Get(leaver, "app", fp); left <- err }()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			got, err := c.Get(context.Background(), "app", fp)
			if err == nil && got != text {
				err = errors.New("wrong text")
			}
			errs <- err
		})
	}
	for c.Stats().Coalesced < 8 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Fatalf("the leaving caller: %v", err)
	}
	release()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if s.Calls() != 1 || c.Stats().Loads != 1 {
		t.Fatalf("calls %d, stats %+v", s.Calls(), c.Stats())
	}
}

// Distinct fetches are bounded: past MaxInFlight they are refused at once, never queued (Release It B1).
func TestInFlightBound(t *testing.T) {
	s := schemaregtest.New(t)
	fp1, fp2 := s.Add("app", `{"a":{}}`), s.Add("app", `{"b":{}}`)
	c := client(t, s, func(cfg *schemareg.Config) { cfg.MaxInFlight = 1 })
	release := s.Block()
	done := make(chan error, 1)
	go func() { _, err := c.Get(context.Background(), "app", fp1); done <- err }()
	for c.Stats().Loads < 1 {
		time.Sleep(time.Millisecond)
	}
	if _, err := c.Get(context.Background(), "app", fp2); !errors.Is(err, schemareg.ErrUnavailable) {
		t.Fatalf("second distinct fetch: %v", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "app", fp2); err != nil {
		t.Fatalf("after the first finished: %v", err)
	}
}

func TestNewClientRefusesABadURL(t *testing.T) {
	for _, u := range []string{"", "controld:7443", "ftp://x"} {
		if _, err := schemareg.NewClient(schemareg.Config{URL: u}); err == nil {
			t.Errorf("url %q accepted", u)
		}
	}
}
