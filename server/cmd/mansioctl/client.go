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
)

// maxAnswer bounds an answer read: a full tenant listing (10 000 tenants by default) is a few MiB.
const maxAnswer = 16 << 20

// client calls the admin API at base with the admin token.
type client struct {
	hc    *http.Client
	base  *url.URL
	token string
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

// call sends method path (with body as JSON when not nil) and decodes the answer's result into out.
func (c *client) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cannot encode the request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	// path is already escaped (url.PathEscape on each variable segment).
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, rd)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is in our message already
		}
		return fmt.Errorf("%s %s: no answer from %s: %w", method, path, c.base.Host, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("%s %s: answer from %s cut off: %w", method, path, c.base.Host, err)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Tag string `json:"tag"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &env)
	if res.StatusCode/100 != 2 {
		e := &apiError{method: method, path: path, status: res.StatusCode}
		if decodeErr == nil && env.Error != nil && tagRE.MatchString(env.Error.Tag) {
			e.tag = env.Error.Tag
		}
		return e
	}
	if decodeErr != nil || len(env.Result) == 0 {
		return fmt.Errorf("%s %s: HTTP %d without an admin API answer ({\"result\": ...}); is -addr the admin API?", method, path, res.StatusCode)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("%s %s: unexpected answer shape: %w", method, path, err)
	}
	return nil
}
