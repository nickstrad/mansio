package schemareg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/panicx"
)

// Config is one Client. Zero fields take the defaults (Release It §P, "Schema registry" sheet).
type Config struct {
	URL   string // controld's API base URL, e.g. https://controld:7443 (required)
	Token string // the control token (Authorization: Bearer); controld accepts it for schema reads
	// HTTP makes the requests; default a client with no timeout of its own (every fetch has a ctx deadline).
	// Give it the private CA's transport (tlsx.HTTPClient) for an https URL.
	HTTP        *http.Client
	Timeout     time.Duration // one fetch, detached from any caller; default 3 s
	MaxEntries  int           // schemas cached; default 1024
	MaxBytes    int64         // bytes of schema text cached; default 64 MiB
	MaxInFlight int           // distinct fetches at once (more are refused, never queued); default 16
	Breaker     breaker.Config
	// Validate, if set, runs after Check on every fetched text before it is cached or returned: IR-2b's
	// docschema check (a valid schema, in fingerprint form). Its error is the registry's fault (it stored a
	// text this agent cannot use): ErrUnavailable, counted as a failure.
	Validate func(fingerprint string, text []byte) error
	Clock    det.Clock // fetch deadlines and the breaker's window; default det.System()
}

func (c *Config) normalize() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("schema registry url %q must be an http(s) URL", c.URL)
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	if c.HTTP == nil {
		c.HTTP = &http.Client{}
	}
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Second
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = 1024
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 64 << 20
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 16
	}
	c.Clock = det.Or(c.Clock)
	return c.Breaker.Normalize()
}

// Stats are the client's counters since start.
type Stats struct {
	Hits      int64 // answered from the cache
	Loads     int64 // fetches started
	Coalesced int64 // callers that joined a fetch in flight
	Failures  int64 // fetches that failed as unavailable (not "not found")
	Shed      int64 // refused: MaxInFlight fetches were running, or the breaker was open
	Entries   int   // schemas cached now
	Bytes     int64 // bytes cached now
}

// Client fetches schemas by (app, fingerprint) and caches them. Safe for concurrent use.
//
//	Invariant: len(flights) <= MaxInFlight
type Client struct {
	cfg Config
	b   *breaker.Breaker

	mu      sync.Mutex
	cache   *cache
	flights map[cacheKey]*flight
	stats   Stats
}

// flight is one fetch in progress; done closes after text, err and panicked are set.
type flight struct {
	done     chan struct{}
	text     string
	err      error
	panicked *panicx.Panic
}

// NewClient validates cfg and returns a client. It does no I/O.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.normalize(); err != nil {
		return nil, errs.Wrap(module, err, "invalid schema registry client config")
	}
	b, err := breaker.New(cfg.Breaker)
	if err != nil {
		return nil, errs.Wrap(module, err, "invalid schema registry breaker config")
	}
	return &Client{cfg: cfg, b: b, cache: newCache(cfg.MaxEntries, cfg.MaxBytes), flights: map[cacheKey]*flight{}}, nil
}

// Breaker is the registry's circuit breaker, for metrics and runtime controls (register it as "schemas").
func (c *Client) Breaker() *breaker.Breaker { return c.b }

// Stats returns the counters.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries, s.Bytes = c.cache.len(), c.cache.bytes
	return s
}

// Get returns the text of the schema app registered under fingerprint: from the cache, or by one fetch shared
// by every concurrent caller of the same key. The fetch runs under Config.Timeout whatever ctx does, so a
// caller that leaves does not cancel it for the others; ctx bounds only this caller's wait (Release It T5).
//
//	post: err == nil ⇒ Check(fingerprint, text) == nil (and Config.Validate accepted it)
//	post: errors match ErrInvalid (bad fingerprint or app; nothing sent), ErrNotFound, ErrUnavailable (with a
//	      RetryAfter hint when the breaker refused) or ctx's error
func (c *Client) Get(ctx context.Context, app, fingerprint string) (string, error) {
	if app == "" || !ValidFingerprint(fingerprint) {
		return "", errs.Wrap(module, ErrInvalid, "cannot fetch schema %s of app %q: malformed; nothing sent", fingerprint, app)
	}
	k := cacheKey{app, fingerprint}
	c.mu.Lock()
	if text, ok := c.cache.get(k); ok {
		c.stats.Hits++
		c.mu.Unlock()
		return text, nil
	}
	f, joined := c.flights[k]
	if joined {
		c.stats.Coalesced++
	} else {
		var err error
		if f, err = c.start(k); err != nil {
			c.stats.Shed++
			c.mu.Unlock()
			return "", err
		}
	}
	c.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return "", errs.Wrap(module, ctx.Err(), "cannot fetch schema %s of app %q: the caller gave up; the fetch goes on for others", fingerprint, app)
	}
	if f.panicked != nil {
		panic(f.panicked) // a bug in the fetch: re-raised in each caller, with the fetching goroutine's stack
	}
	return f.text, f.err
}

// start begins fetching k.
//
//	pre: c.mu held; k is neither cached nor in flight
func (c *Client) start(k cacheKey) (*flight, error) {
	if len(c.flights) >= c.cfg.MaxInFlight {
		return nil, errs.Wrap(module, retryAfter{ErrUnavailable, time.Second},
			"cannot fetch schema %s of app %q: %d fetches in flight; nothing sent, retry shortly", k.fingerprint, k.app, len(c.flights))
	}
	permit, wait, ok := c.b.Allow(c.cfg.Clock.Now())
	if !ok {
		return nil, errs.Wrap(module, retryAfter{ErrUnavailable, wait},
			"cannot fetch schema %s of app %q: the registry circuit breaker is open; nothing sent, retry after %v", k.fingerprint, k.app, wait)
	}
	f := &flight{done: make(chan struct{})}
	c.flights[k] = f
	c.stats.Loads++
	go c.load(k, f, permit)
	return f, nil
}

// load runs one fetch and publishes its result to every waiter.
func (c *Client) load(k cacheKey, f *flight, permit breaker.Permit) {
	outcome := breaker.Failure
	defer func() {
		if p := panicx.From(recover()); p != nil {
			f.panicked = p
		}
		c.b.Done(permit, c.cfg.Clock.Now(), outcome)
		c.mu.Lock()
		delete(c.flights, k)
		switch {
		case f.panicked == nil && f.err == nil:
			c.cache.put(k, f.text)
		case errors.Is(f.err, ErrUnavailable):
			c.stats.Failures++
		}
		c.mu.Unlock()
		close(f.done)
	}()
	ctx, cancel := det.WithTimeout(context.Background(), c.cfg.Clock, c.cfg.Timeout)
	defer cancel()
	text, err := c.fetch(ctx, k)
	if err == nil || errors.Is(err, ErrNotFound) {
		outcome = breaker.Success // an answer, either way (Release It C2)
	}
	if err != nil {
		f.err = errs.Wrap(module, err, "cannot fetch schema %s of app %q", k.fingerprint, k.app)
		return
	}
	f.text = text
}

// answer is the body of GET /v1/apps/{app}/schemas/{fingerprint}: the text as a JSON string, so it arrives
// byte for byte whatever the encoder escapes.
type answer struct {
	Code        string `json:"code"`
	App         string `json:"app"`
	Fingerprint string `json:"fingerprint"`
	Schema      string `json:"schema"`
}

// maxAnswer bounds the answer: the text JSON-escaped (≤ 6 bytes per byte for \u00XX) plus the envelope.
const maxAnswer = 6*MaxText + 4<<10

// fetch GETs one schema and checks it. Errors match ErrNotFound (404) or ErrUnavailable (everything else).
func (c *Client) fetch(ctx context.Context, k cacheKey) (string, error) {
	u := c.cfg.URL + "/v1/apps/" + url.PathEscape(k.app) + "/schemas/" + url.PathEscape(k.fingerprint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	res, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer+1))
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: reading the answer: %w", ErrUnavailable, err)
	case res.StatusCode == http.StatusNotFound:
		return "", ErrNotFound
	case res.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: the registry answered %d", ErrUnavailable, res.StatusCode)
	case len(body) > maxAnswer:
		return "", fmt.Errorf("%w: the answer is over %d bytes", ErrUnavailable, maxAnswer)
	}
	var a answer
	if err := json.Unmarshal(body, &a); err != nil {
		return "", fmt.Errorf("%w: undecodable answer: %w", ErrUnavailable, err)
	}
	if a.App != k.app || a.Fingerprint != k.fingerprint {
		return "", fmt.Errorf("%w: answered for %q %s, asked for %q %s", ErrUnavailable, a.App, a.Fingerprint, k.app, k.fingerprint)
	}
	// Integrity (object-storage guide V, applied to the registry): never use a text that is not the one named.
	if err := Check(k.fingerprint, []byte(a.Schema)); err != nil {
		return "", fmt.Errorf("%w: the registry's text fails its check: %w", ErrUnavailable, err)
	}
	if c.cfg.Validate != nil {
		if err := c.cfg.Validate(k.fingerprint, []byte(a.Schema)); err != nil {
			return "", fmt.Errorf("%w: the registry's text is not a usable schema: %w", ErrUnavailable, err)
		}
	}
	return a.Schema, nil
}

// retryAfter is an error with a retry hint (front.RetryHint).
type retryAfter struct {
	err  error
	wait time.Duration
}

func (e retryAfter) Error() string             { return e.err.Error() }
func (e retryAfter) Unwrap() error             { return e.err }
func (e retryAfter) RetryAfter() time.Duration { return e.wait }
