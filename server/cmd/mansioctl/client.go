package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/nickstrad/mansio/server/internal/front"
)

// maxAnswer bounds an answer read: a full tenant listing (10 000 tenants by default) is a few MiB.
const maxAnswer = 16 << 20

// client calls one listener: an API at base with the admin token, or an agent's admin listener (no token).
type client struct {
	hc      *http.Client
	base    string // scheme://host[/path], no trailing slash
	host    string // for messages
	token   string
	timeout time.Duration // the command's deadline (sent to an agent as X-Timeout-Ms)
}

// apiError is an error answer: its status and, when the body is the admin API's {"error": {"tag"}}, its tag.
// The rest of the body is never printed: it is the server's text, not ours.
type apiError struct {
	method, path string
	status       int
	tag          string
}

func (e *apiError) Error() string {
	what := fmt.Sprintf("%s %s: HTTP %d", e.method, e.path, e.status)
	if e.tag != "" {
		what += " " + e.tag
	}
	return what + ": " + e.hint()
}

// hint says what an error answer means for the operator.
func (e *apiError) hint() string {
	switch {
	case e.status == http.StatusUnauthorized:
		return "the admin token was refused; check -token-file or ADMIN_TOKEN"
	case e.status == http.StatusNotFound && e.tag == "RouteNotFound":
		return "the server has no such admin route (tenancy off, or -addr is not the admin API)"
	case e.status == http.StatusNotFound:
		return "not found"
	case e.status == http.StatusBadRequest:
		return "the server refused the input as invalid"
	case e.status == http.StatusConflict:
		return "conflict with an existing record (e.g. the slug is taken)"
	case e.status == http.StatusForbidden && e.tag == "TenantDeactivated":
		return "the tenant is deactivated"
	case e.status == http.StatusServiceUnavailable:
		return "the server or its store is unavailable or busy; retry"
	case e.status == http.StatusGatewayTimeout:
		return "the call ran out of time on the server; retry"
	case e.status >= 500:
		return "the server failed; its log has the details"
	case e.status/100 == 3:
		return "a redirect, not followed (the token goes only to -addr); point -addr at the admin API itself"
	}
	return "the call failed"
}

// tagRE is what a tag must look like to be printed.
var tagRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,39}$`)

// send makes one request to base+path (path already escaped: url.PathEscape on each variable segment) and
// returns the status and at most limit bytes of the answer. body, if not nil, is sent as it is.
func (c *client) send(ctx context.Context, method, path string, body []byte, header http.Header, limit int64) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is in our message already
		}
		return 0, nil, fmt.Errorf("%s %s: no answer from %s: %w", method, path, c.host, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: answer from %s cut off: %w", method, path, c.host, err)
	}
	return res.StatusCode, raw, nil
}

// call sends method path to the tenant admin API (with body as JSON when not nil) and decodes the answer's
// result into out.
func (c *client) call(ctx context.Context, method, path string, body, out any) error {
	header := http.Header{"Accept": {"application/json"}}
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return fmt.Errorf("cannot encode the request: %w", err)
		}
		header.Set("Content-Type", "application/json")
	}
	status, raw, err := c.send(ctx, method, path, b, header, maxAnswer)
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Tag string `json:"tag"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &env)
	if status/100 != 2 {
		e := &apiError{method: method, path: path, status: status}
		if decodeErr == nil && env.Error != nil && tagRE.MatchString(env.Error.Tag) {
			e.tag = env.Error.Tag
		}
		return e
	}
	if decodeErr != nil || len(env.Result) == 0 {
		return fmt.Errorf("%s %s: HTTP %d without an admin API answer ({\"result\": ...}); is -addr the admin API?", method, path, status)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("%s %s: unexpected answer shape: %w", method, path, err)
	}
	return nil
}

// maxRawAnswer bounds a controld API answer.
const maxRawAnswer = 1 << 20

// raw sends method path to controld's API (body as JSON when not nil) and returns the answer as it came, to be
// printed whatever the status; an answer other than 2xx is also an error.
func (c *client) raw(ctx context.Context, method, path string, body any) (view, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return view{}, fmt.Errorf("cannot encode the request: %w", err)
		}
	}
	status, out, err := c.send(ctx, method, path, b, nil, maxRawAnswer)
	if err != nil {
		return view{}, err
	}
	if status/100 != 2 {
		return view{raw: out}, fmt.Errorf("%s %s: HTTP %d (the answer is on stdout)", method, path, status)
	}
	return view{raw: out}, nil
}

// maxAgentAnswer bounds an agent admin answer (a few hundred bytes).
const maxAgentAnswer = 64 << 10

// agentAnswer is an agent admin listener's JSON answer: code, and message unless ok.
type agentAnswer struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// agent POSTs path to the agent's admin listener with the deadline in X-Timeout-Ms. The answer is printed as
// one line; a non-JSON answer, or one without a code, is an error, and so is any code but ok.
func (c *client) agent(ctx context.Context, path string) (view, error) {
	header := http.Header{front.TimeoutHeader: {strconv.FormatInt(c.timeout.Milliseconds(), 10)}}
	status, raw, err := c.send(ctx, http.MethodPost, path, nil, header, maxAgentAnswer)
	if err != nil {
		return view{}, err
	}
	var a agentAnswer
	if err := json.Unmarshal(raw, &a); err != nil || a.Code == "" {
		return view{}, fmt.Errorf("%s answered %d %s with no JSON code: %q", c.host, status, http.StatusText(status), bytes.TrimSpace(raw))
	}
	v := view{raw: append(bytes.TrimSpace(raw), '\n')}
	if a.Code != "ok" {
		return v, fmt.Errorf("%s: %s", a.Code, a.Message)
	}
	return v, nil
}
