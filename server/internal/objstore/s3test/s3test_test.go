package s3test_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/objstore/s3test"
)

func newServer(t *testing.T) (*s3test.Server, string) {
	t.Helper()
	s := s3test.New()
	t.Cleanup(s.Close)
	return s, "http://" + s.Endpoint() + "/bkt/"
}

func do(t *testing.T, c *http.Client, method, url string, body []byte, hdr map[string]string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res, b, err
}

func TestHealthyObjectRoundTripAndConditions(t *testing.T) {
	_, base := newServer(t)
	c := http.DefaultClient
	if res, _, _ := do(t, c, "GET", base+"k", nil, nil); res.StatusCode != 404 {
		t.Fatalf("missing GET: %d", res.StatusCode)
	}
	res, _, _ := do(t, c, "PUT", base+"k", []byte("v1"), map[string]string{"If-None-Match": "*", "X-Amz-Meta-Sha256": "abc"})
	if res.StatusCode != 200 || res.Header.Get("ETag") != `"`+s3test.ETag([]byte("v1"))+`"` {
		t.Fatalf("create: %d %q", res.StatusCode, res.Header.Get("ETag"))
	}
	if res, _, _ := do(t, c, "PUT", base+"k", []byte("v2"), map[string]string{"If-None-Match": "*"}); res.StatusCode != 412 {
		t.Fatalf("second create: %d, want 412", res.StatusCode)
	}
	if res, _, _ := do(t, c, "PUT", base+"k", []byte("v2"), map[string]string{"If-Match": `"stale"`}); res.StatusCode != 412 {
		t.Fatalf("stale If-Match: %d, want 412", res.StatusCode)
	}
	res, b, _ := do(t, c, "GET", base+"k", nil, nil)
	if string(b) != "v1" || res.Header.Get("X-Amz-Meta-Sha256") != "abc" {
		t.Fatalf("GET = %q meta %q", b, res.Header.Get("X-Amz-Meta-Sha256"))
	}
	if res, _, _ := do(t, c, "DELETE", base+"k", nil, nil); res.StatusCode != 204 {
		t.Fatalf("DELETE: %d", res.StatusCode)
	}
}

func TestContentMD5MismatchIsBadDigestAndStoresNothing(t *testing.T) {
	s, base := newServer(t)
	sum := md5.Sum([]byte("other"))
	res, b, _ := do(t, http.DefaultClient, "PUT", base+"k", []byte("body"),
		map[string]string{"Content-Md5": base64.StdEncoding.EncodeToString(sum[:])})
	if res.StatusCode != 400 || !strings.Contains(string(b), "BadDigest") {
		t.Fatalf("PUT: %d %s", res.StatusCode, b)
	}
	if _, ok := s.Object("bkt", "k"); ok {
		t.Fatal("a BadDigest PUT stored the object")
	}
}

func TestStatusFailsNRequestsThenHeals(t *testing.T) {
	s, base := newServer(t)
	s.Put("bkt", "k", []byte("v"))
	s.Inject(s3test.Fault{Mode: s3test.Status, Times: 2})
	for i, want := range []int{503, 503, 200, 200} {
		res, b, _ := do(t, http.DefaultClient, "GET", base+"k", nil, nil)
		if res.StatusCode != want {
			t.Fatalf("request %d: %d, want %d", i, res.StatusCode, want)
		}
		if want == 503 && !strings.Contains(string(b), "SlowDown") {
			t.Fatalf("503 body %s", b)
		}
	}
	if n := s.Count("GET"); n != 4 {
		t.Fatalf("Count(GET) = %d", n)
	}
}

func TestMethodFilter(t *testing.T) {
	s, base := newServer(t)
	s.Put("bkt", "k", []byte("v"))
	s.Inject(s3test.Fault{Mode: s3test.Status, Status: 500, Method: "PUT", Times: 1})
	if res, _, _ := do(t, http.DefaultClient, "GET", base+"k", nil, nil); res.StatusCode != 200 {
		t.Fatalf("GET hit a PUT-only fault: %d", res.StatusCode)
	}
	if res, _, _ := do(t, http.DefaultClient, "PUT", base+"k", []byte("x"), nil); res.StatusCode != 500 {
		t.Fatalf("PUT: %d, want 500", res.StatusCode)
	}
}

func TestBodyFaults(t *testing.T) {
	data := []byte("0123456789abcdef")
	for _, tc := range []struct {
		mode    s3test.Mode
		wantErr bool
		check   func([]byte) bool
	}{
		{s3test.TruncateBody, true, func(b []byte) bool { return len(b) < len(data) }},
		{s3test.WrongContentLength, false, func(b []byte) bool { return bytes.Equal(b, data[:len(data)-1]) }},
		{s3test.CorruptBody, false, func(b []byte) bool { return len(b) == len(data) && !bytes.Equal(b, data) }},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			s, base := newServer(t)
			s.Put("bkt", "k", data)
			s.Inject(s3test.Fault{Mode: tc.mode, Times: 1})
			_, b, err := do(t, http.DefaultClient, "GET", base+"k", nil, nil)
			if (err != nil) != tc.wantErr || !tc.check(b) {
				t.Fatalf("body %q err %v", b, err)
			}
			if _, b, err := do(t, http.DefaultClient, "GET", base+"k", nil, nil); err != nil || !bytes.Equal(b, data) {
				t.Fatalf("after one fault: %q %v", b, err)
			}
		})
	}
}

func TestHeadersThenStallSendsHeadersOnly(t *testing.T) {
	s, base := newServer(t)
	s.Put("bkt", "k", []byte("data"))
	s.Inject(s3test.Fault{Mode: s3test.HeadersThenStall})
	c := &http.Client{Timeout: 300 * time.Millisecond}
	req, _ := http.NewRequest("GET", base+"k", nil)
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("headers did not arrive: %v", err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Fatal("body read finished on a stalled response")
	}
}

func TestDribbleSendsAtTheRate(t *testing.T) {
	s, base := newServer(t)
	s.Put("bkt", "k", []byte("abcde"))
	s.Inject(s3test.Fault{Mode: s3test.Dribble, Rate: 50})
	t0 := time.Now()
	_, b, err := do(t, http.DefaultClient, "GET", base+"k", nil, nil)
	if err != nil || string(b) != "abcde" {
		t.Fatalf("%q %v", b, err)
	}
	if d := time.Since(t0); d < 80*time.Millisecond { // 5 bytes at 50 B/s ≈ 100ms
		t.Fatalf("5 bytes at 50 B/s took %v", d)
	}
}

func TestApplyThenDropReplyAppliesTheWrite(t *testing.T) {
	s, base := newServer(t)
	s.Inject(s3test.Fault{Mode: s3test.ApplyThenDropReply, Times: 1})
	_, _, err := do(t, http.DefaultClient, "PUT", base+"k", []byte("v"), map[string]string{"If-None-Match": "*"})
	if err == nil {
		t.Fatal("the reply was not dropped")
	}
	if b, ok := s.Object("bkt", "k"); !ok || string(b) != "v" {
		t.Fatalf("the write did not apply: %q %v", b, ok)
	}
}

func TestCloseReleasesStalledHandlers(t *testing.T) {
	s := s3test.New()
	s.Put("bkt", "k", []byte("data"))
	s.Inject(s3test.Fault{Mode: s3test.HeadersThenStall})
	done := make(chan error, 1)
	go func() {
		res, err := http.Get("http://" + s.Endpoint() + "/bkt/k")
		if err == nil {
			_, err = io.ReadAll(res.Body)
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung on a stalled handler")
	}
	if err := <-done; err == nil {
		t.Fatal("stalled GET finished cleanly")
	}
}
