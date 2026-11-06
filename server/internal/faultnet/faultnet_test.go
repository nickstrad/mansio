package faultnet_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/faultnet"
)

// echo is an upstream that echoes every byte and counts its connections.
type echo struct {
	ln    net.Listener
	conns atomic.Int64
}

func newEcho(t *testing.T) *echo {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &echo{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			e.conns.Add(1)
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return e
}

func newProxy(t *testing.T) (*faultnet.Proxy, *echo) {
	t.Helper()
	e := newEcho(t)
	p, err := faultnet.New(e.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, e
}

func dial(t *testing.T, p *faultnet.Proxy) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", p.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// dialRoundTrip dials p and round-trips msg; a refused connection may fail already in the dial (the proxy's
// reset can beat connect() under load), which counts as the round trip failing.
func dialRoundTrip(t *testing.T, p *faultnet.Proxy, msg []byte, d time.Duration) ([]byte, error) {
	c, err := net.DialTimeout("tcp", p.Addr(), d)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { c.Close() })
	return roundTrip(c, msg, d)
}

// roundTrip writes msg and reads len(msg) bytes back within d.
func roundTrip(c net.Conn, msg []byte, d time.Duration) ([]byte, error) {
	c.SetDeadline(time.Now().Add(d))
	if _, err := c.Write(msg); err != nil {
		return nil, err
	}
	got := make([]byte, len(msg))
	n, err := io.ReadFull(c, got)
	return got[:n], err
}

func isTimeout(err error) bool { return errors.Is(err, os.ErrDeadlineExceeded) }

func TestPassForwardsBothWays(t *testing.T) {
	p, e := newProxy(t)
	msg := bytes.Repeat([]byte("abc"), 100_000)
	got, err := roundTrip(dial(t, p), msg, 5*time.Second)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("round trip: %d bytes, err %v", len(got), err)
	}
	if e.conns.Load() != 1 || p.Accepted() != 1 {
		t.Fatalf("upstream conns %d, accepted %d", e.conns.Load(), p.Accepted())
	}
}

func TestRefuseFailsFast(t *testing.T) {
	p, e := newProxy(t)
	p.Set(faultnet.RefuseConn(), 0)
	t0 := time.Now()
	_, err := dialRoundTrip(t, p, []byte("x"), 2*time.Second)
	if err == nil || isTimeout(err) {
		t.Fatalf("err = %v, want a reset", err)
	}
	// Fast = well inside the 2 s read budget (a refusal that waited for it would also be a timeout, above).
	// Not 100 ms: that flaked under load (CP-8).
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("refusal took %v, want well under the 2s read budget", d)
	}
	if e.conns.Load() != 0 {
		t.Fatal("a refused connection reached upstream")
	}
}

func TestSilenceAndBlackholeNeverReply(t *testing.T) {
	for _, f := range []faultnet.Fault{faultnet.Silence(), faultnet.Hole()} {
		t.Run(f.Kind.String(), func(t *testing.T) {
			p, e := newProxy(t)
			p.Set(f, 0)
			if _, err := roundTrip(dial(t, p), []byte("hello"), 200*time.Millisecond); !isTimeout(err) {
				t.Fatalf("err = %v, want a read timeout", err)
			}
			if e.conns.Load() != 0 {
				t.Fatal("upstream saw a connection")
			}
		})
	}
}

func TestResetAfterCutsTheReplyAtTheBudget(t *testing.T) {
	for _, n := range []int64{0, 1, 1000} {
		p, _ := newProxy(t)
		p.Set(faultnet.ResetAfterBytes(n), 0)
		got, err := dialRoundTrip(t, p, make([]byte, 4096), 2*time.Second)
		if err == nil || isTimeout(err) {
			t.Fatalf("n=%d: err = %v, want a reset", n, err)
		}
		if int64(len(got)) > n {
			t.Fatalf("n=%d: got %d bytes past the budget", n, len(got))
		}
	}
}

func TestLatencyDelaysEachDirection(t *testing.T) {
	p, _ := newProxy(t)
	p.Set(faultnet.Delayed(50*time.Millisecond), 0)
	t0 := time.Now()
	if _, err := roundTrip(dial(t, p), []byte("x"), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < 100*time.Millisecond {
		t.Fatalf("round trip took %v, want >= 2 × 50ms", d)
	}
}

func TestThrottleBoundsTheRate(t *testing.T) {
	p, _ := newProxy(t)
	p.Set(faultnet.Throttled(40_000), 0)
	t0 := time.Now()
	got, err := roundTrip(dial(t, p), make([]byte, 10_000), 5*time.Second)
	if err != nil || len(got) != 10_000 {
		t.Fatalf("%d bytes, err %v", len(got), err)
	}
	if d := time.Since(t0); d < 200*time.Millisecond { // 10 kB each way at 40 kB/s ≥ 250ms
		t.Fatalf("10 kB took %v at 40 kB/s", d)
	}
}

func TestHalfOpenFreezesLiveConnectionsForGood(t *testing.T) {
	p, _ := newProxy(t)
	old := dial(t, p)
	if _, err := roundTrip(old, []byte("before"), time.Second); err != nil {
		t.Fatal(err)
	}
	p.Set(faultnet.Frozen(), 0)
	p.Heal() // only the connections open at Set are frozen
	if _, err := roundTrip(old, []byte("after"), 200*time.Millisecond); !isTimeout(err) {
		t.Fatalf("frozen connection: err = %v, want a timeout", err)
	}
	if _, err := roundTrip(dial(t, p), []byte("new"), time.Second); err != nil {
		t.Fatalf("new connection after Heal: %v", err)
	}
}

func TestHalfOpenNewConnectionsHang(t *testing.T) {
	p, e := newProxy(t)
	p.Set(faultnet.Frozen(), 0)
	if _, err := roundTrip(dial(t, p), []byte("x"), 200*time.Millisecond); !isTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if e.conns.Load() != 0 {
		t.Fatal("a half-open connection reached upstream")
	}
}

// Every fault is a counter-limited switch: fail N connections, then heal on its own.
func TestFaultAppliesToTheNextNConnections(t *testing.T) {
	for _, n := range []int{1, 2} {
		p, _ := newProxy(t)
		p.Set(faultnet.RefuseConn(), n)
		for i := range n + 2 {
			_, err := dialRoundTrip(t, p, []byte("x"), time.Second)
			if failed := err != nil; failed != (i < n) {
				t.Fatalf("n=%d conn %d: err = %v", n, i, err)
			}
		}
	}
}

func TestCloseEndsHeldConnections(t *testing.T) {
	p, _ := newProxy(t)
	p.Set(faultnet.Silence(), 0)
	c := dial(t, p)
	for deadline := time.Now().Add(5 * time.Second); p.Accepted() == 0; { // let the proxy accept it
		if time.Now().After(deadline) {
			t.Fatal("the proxy never accepted the connection")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { _, err := roundTrip(c, []byte("x"), 5*time.Second); done <- err }()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read succeeded on a closed proxy")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close left a held connection open")
	}
	if _, err := net.DialTimeout("tcp", p.Addr(), 200*time.Millisecond); err == nil {
		t.Fatal("closed proxy still accepts")
	}
}

func TestFlipByteCorruptsOneRequestByte(t *testing.T) {
	for _, off := range []int64{0, 5, 9} {
		p, _ := newProxy(t)
		p.Set(faultnet.FlipByteAt(off), 0)
		msg := []byte("0123456789")
		got, err := roundTrip(dial(t, p), msg, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for i := range msg {
			if flipped := got[i] != msg[i]; flipped != (int64(i) == off) || flipped && got[i] != msg[i]^0xff {
				t.Fatalf("offset %d: got %q", off, got)
			}
		}
	}
	p, _ := newProxy(t)
	p.Set(faultnet.FlipByteAt(10), 0) // off point: just past the message
	if got, err := roundTrip(dial(t, p), []byte("0123456789"), time.Second); err != nil || string(got) != "0123456789" {
		t.Fatalf("got %q %v", got, err)
	}
}
