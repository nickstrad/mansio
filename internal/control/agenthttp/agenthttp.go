// Package agenthttp is the control plane's client for one agent's lifecycle endpoints (cmd/agentd
// control.go): Claim, Fence, Forget and Health over HTTP with the control token. An adapter: it translates
// the wire codes to owner's sentinels, bounds every call by a deadline and guards each agent with its own
// circuit breaker (Release It T1, C1). It never retries: the lifecycle controller's resumer is the one
// retry layer (Release It R1).
//
// Errors are wrapped for module "agenthttp". An agent's answer maps to owner.ErrWrongOwner, ErrMoving,
// ErrFenced, ErrIllegal or ErrInvalid; anything that says nothing about the key (transport errors, a
// timeout, 503 unavailable, an open breaker, 500) matches ErrUnavailable, and the call's outcome is
// unknown (Claim and Fence are idempotent: call again).
package agenthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/owner"
)

const module = "agenthttp"

// ErrUnavailable: the agent did not answer about the key (unreachable, timed out, overloaded, a dependency
// of its own down, or its breaker here is open). Retry later; the call may have applied.
var ErrUnavailable = errors.New("agenthttp: agent unavailable")

// ErrUnauthorized: the agent refused the control token (a configuration error; retrying does not help).
var ErrUnauthorized = errors.New("agenthttp: control token refused")

// The agent's lifecycle paths (cmd/agentd control.go).
const (
	claimPath  = "/v1/lifecycle/claim"
	fencePath  = "/v1/lifecycle/fence"
	forgetPath = "/v1/lifecycle/forget"
	healthPath = "/v1/lifecycle/health"
)

// Config is one agent's client.
type Config struct {
	VM      string
	BaseURL string // the agent's lifecycle listener, e.g. http://10.0.0.5:7071
	Token   string // the control token (the agent's secrets file)
	// Timeout bounds each call (and is sent as X-Timeout-Ms so the agent stops when we do). Default 30 s:
	// a fence may write a database back to S3.
	Timeout time.Duration
	// Breaker is this agent's breaker (zero value: RI §P defaults). Set OnChange to log transitions.
	Breaker breaker.Config
	// HTTP is the shared client (one per process; its transport pools connections). Default: a client
	// with dial and header timeouts.
	HTTP *http.Client
	Now  func() time.Time
}

// Client calls one agent. Safe for concurrent use.
type Client struct {
	cfg Config
	b   *breaker.Breaker
}

// New validates cfg and builds the client. No I/O.
func New(cfg Config) (*Client, error) {
	if cfg.VM == "" || cfg.BaseURL == "" || cfg.Token == "" {
		return nil, errs.Wrap(module, nil, "agent client: vm, base url and token are required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.HTTP == nil {
		cfg.HTTP = DefaultHTTP()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	b, err := breaker.New(cfg.Breaker)
	if err != nil {
		return nil, errs.Wrap(module, err, "agent client %s: invalid breaker config", cfg.VM)
	}
	return &Client{cfg: cfg, b: b}, nil
}

// DefaultHTTP is a client whose transport bounds connecting and waiting for headers; each call's ctx bounds
// the rest.
func DefaultHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 0 // the call's deadline covers it (a fence may take long)
	t.MaxIdleConnsPerHost = 16
	return &http.Client{Transport: t}
}

// Breaker is this agent's breaker (metrics, logs).
func (c *Client) Breaker() *breaker.Breaker { return c.b }

// VM is the agent's VM id.
func (c *Client) VM() string { return c.cfg.VM }

// answer is every lifecycle answer: ok fields, or an error code and message.
type answer struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	LogID   string `json:"log_id"`
	DB      string `json:"db"`
	State   string `json:"state"`
	Epoch   uint64 `json:"epoch"`
	Gen     uint64 `json:"gen"`
}

// Claim asks the agent to claim key as the authority says (flow B/C).
func (c *Client) Claim(ctx context.Context, key string) (owner.Local, error) {
	a, err := c.call(ctx, http.MethodPost, claimPath, key)
	if err != nil {
		return owner.Local{}, err
	}
	l := owner.Local{Epoch: owner.Epoch(a.Epoch), Gen: owner.Generation(a.Gen)}
	if a.State == owner.Active.String() {
		l.State = owner.Active
	}
	return l, nil
}

// Fence asks the agent to hand key off: stop, publish, durable FENCED. Idempotent.
func (c *Client) Fence(ctx context.Context, key string) (owner.Ack, error) {
	a, err := c.call(ctx, http.MethodPost, fencePath, key)
	if err != nil {
		return owner.Ack{}, err
	}
	return owner.Ack{Key: a.DB, Epoch: owner.Epoch(a.Epoch), Gen: owner.Generation(a.Gen)}, nil
}

// Forget asks the agent to drop a fenced key's record.
func (c *Client) Forget(ctx context.Context, key string) error {
	_, err := c.call(ctx, http.MethodPost, forgetPath, key)
	return err
}

// Health is the part of the agent's health answer the control plane reads.
type Health struct {
	VM     string         `json:"vm"`
	Owned  map[string]int `json:"owned"`
	Active int            `json:"active"`
	Bytes  int64          `json:"bytes"`
}

// Health reads the agent's health answer (controld's heartbeat).
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, healthPath, "", nil, func(body []byte) error { return json.Unmarshal(body, &h) })
	return h, err
}

func (c *Client) call(ctx context.Context, method, path, key string) (answer, error) {
	var a answer
	body, _ := json.Marshal(map[string]string{"db": key})
	err := c.do(ctx, method, path, key, body, func(b []byte) error { return json.Unmarshal(b, &a) })
	return a, err
}

// maxAnswer bounds an answer's body.
const maxAnswer = 64 << 10

// do runs one call under the breaker and the deadline, and decodes a 200 answer with ok.
func (c *Client) do(ctx context.Context, method, path, key string, body []byte, ok func([]byte) error) error {
	what := fmt.Sprintf("%s %s on %s", path, strconv.Quote(key), c.cfg.VM)
	if err := ctx.Err(); err != nil {
		return errs.Wrap(module, err, "cannot call %s: the caller gave up; nothing sent", what)
	}
	permit, wait, allowed := c.b.Allow(c.cfg.Now())
	if !allowed {
		return errs.Wrap(module, &openError{wait}, "cannot call %s: the agent's breaker is open; nothing sent, retry after %v", what, wait)
	}
	cctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	status, raw, err := c.roundTrip(cctx, method, path, body)
	outcome := breaker.Success
	switch {
	case err != nil && ctx.Err() != nil:
		outcome = breaker.Ignore // the caller left
	case err != nil, status == http.StatusInternalServerError, status == http.StatusGatewayTimeout:
		outcome = breaker.Failure
	case status == http.StatusServiceUnavailable:
		outcome = breaker.Ignore // the agent answered: its own dependency or slots are busy
	}
	c.b.Done(permit, c.cfg.Now(), outcome)
	if err != nil {
		return errs.Wrap(module, fmt.Errorf("%w: %w", ErrUnavailable, err), "cannot call %s: no answer; it may have applied, call again", what)
	}
	if status == http.StatusOK {
		if err := ok(raw); err != nil {
			return errs.Wrap(module, err, "cannot call %s: the answer does not parse", what)
		}
		return nil
	}
	var a answer
	_ = json.Unmarshal(raw, &a)
	return errs.Wrap(module, codeError(a.Code, status), "%s answered %d %s: %s%s", what, status, a.Code, a.Message, logID(a.LogID))
}

func logID(id string) string {
	if id == "" {
		return ""
	}
	return " (agent log ID " + id + ")"
}

func (c *Client) roundTrip(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	if dl, ok := ctx.Deadline(); ok {
		req.Header.Set("X-Timeout-Ms", strconv.FormatInt(max(1, time.Until(dl).Milliseconds()), 10))
	}
	res, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	return res.StatusCode, raw, err
}

// codeError maps an agent's error code to its sentinel.
func codeError(code string, status int) error {
	switch code {
	case "wrong_owner":
		return owner.ErrWrongOwner
	case "moving":
		return owner.ErrMoving
	case "fenced":
		return owner.ErrFenced
	case "illegal":
		return owner.ErrIllegal
	case "bad_request":
		return owner.ErrInvalid
	case "unauthorized":
		return ErrUnauthorized
	case "unavailable", "timeout", "internal":
		return ErrUnavailable
	}
	return fmt.Errorf("%w: unexpected answer %d %q", ErrUnavailable, status, code)
}

// openError is a call this agent's breaker refused.
type openError struct{ wait time.Duration }

func (e *openError) Error() string { return "agent circuit breaker open" }
func (e *openError) Is(target error) bool {
	return target == ErrUnavailable || target == breaker.ErrOpen
}

// RetryAfter is the breaker's hint.
func (e *openError) RetryAfter() time.Duration { return e.wait }
