package tenancyhttp_test

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/tenancy"
	"github.com/nickstrad/mansio/server/internal/tenancy/tenancyhttp"
)

// stall opens n connections that send path's headers and the first byte of a body, then nothing more: the
// slow client of B2. The caller closes them.
func stall(t *testing.T, addr string, n int, path, auth string) []net.Conn {
	t.Helper()
	conns := make([]net.Conn, n)
	for i := range conns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n%sContent-Length: 200\r\n\r\n{", path, auth)
		conns[i] = c
	}
	time.Sleep(50 * time.Millisecond) // the handlers are now waiting for the rest of the bodies
	return conns
}

// B2's overload scenario: more stalled senders than there are slots, on the sign-in pool and on the admin pool.
// Before, each stalled body held a slot (the slot was taken, then the body read) and the next caller was
// refused 503; now the body is read first, so the stalled senders hold no slot and real calls are served.
func TestStalledBodiesHoldNoSlot(t *testing.T) {
	var shed tenancyhttp.Shed
	srv, _ := rig(t, tenancy.Sessions, func(c *tenancyhttp.Config) { c.Concurrency, c.SignInBurst, c.Shed = 2, 100, &shed })
	addr := strings.TrimPrefix(srv.URL, "http://")
	conns := append(stall(t, addr, 4, "/api/auth/sign-in/email", ""),
		stall(t, addr, 4, "/admin/tenants", "Authorization: Bearer "+admin+"\r\n")...)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	if r := call(t, srv, "POST", "/admin/tenants", admin, map[string]string{"name": "A", "slug": "a"}); r.status != 200 {
		t.Fatalf("admin call beside 4 stalled admin bodies (2 slots): %d %v", r.status, r.body)
	}
	up := call(t, srv, "POST", "/api/auth/sign-up/email", "", map[string]any{"name": "Ann", "email": "ann@example.test", "password": "long enough"})
	if up.status != 200 {
		t.Fatalf("sign-up beside 4 stalled sign-in bodies (2 slots): %d %v", up.status, up.body)
	}
	if a, s := shed.Admin.Load(), shed.Auth.Load(); a != 0 || s != 0 {
		t.Fatalf("refusals counted: admin %d, auth %d; want none", a, s)
	}
}

// A full pool still refuses at once, and the refusal is counted (B1) against its own pool.
func TestFullPoolRefusesAndCounts(t *testing.T) {
	var shed tenancyhttp.Shed
	srv, f := rig(t, tenancy.Sessions, func(c *tenancyhttp.Config) { c.Concurrency, c.Shed = 1, &shed })
	f.Hold = make(chan struct{}) // a slow store: the first call keeps the one admin slot
	done := make(chan reply)
	go func() { done <- call(t, srv, "GET", "/admin/tenants", admin, nil) }()
	time.Sleep(50 * time.Millisecond)
	if r := call(t, srv, "GET", "/admin/tenants", admin, nil); r.status != 503 || r.tag() != "shed" || r.header.Get("Retry-After") == "" {
		t.Fatalf("second admin call with the slot taken: %d %v", r.status, r.body)
	}
	close(f.Hold)
	if r := <-done; r.status != 200 {
		t.Fatalf("the call holding the slot: %d %v", r.status, r.body)
	}
	if shed.Admin.Load() != 1 || shed.Auth.Load() != 0 {
		t.Fatalf("refusals counted: admin %d, auth %d; want 1 and 0", shed.Admin.Load(), shed.Auth.Load())
	}
}
