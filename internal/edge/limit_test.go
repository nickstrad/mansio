package edge

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

// Spec table: a bucket holds burst tokens, refills at rate, and an empty bucket says how long until one is back.
func TestSourceLimiterBucket(t *testing.T) {
	l := NewSourceLimiter(10, 3, 100) // 10/s, burst 3
	for i := range 3 {
		if ok, _ := l.Allow("a", t0); !ok {
			t.Fatalf("token %d of the burst refused", i)
		}
	}
	ok, wait := l.Allow("a", t0) // off point: burst + 1
	if ok || wait != 100*time.Millisecond {
		t.Fatalf("4th at once = %v, wait %v; want refused, 100ms", ok, wait)
	}
	if ok, _ := l.Allow("b", t0); !ok {
		t.Fatal("another source is limited by a's bucket")
	}
	if ok, _ := l.Allow("a", t0.Add(99*time.Millisecond)); ok { // just under one token
		t.Fatal("refilled too early")
	}
	if ok, _ := l.Allow("a", t0.Add(100*time.Millisecond)); !ok { // on point: one token back
		t.Fatal("not refilled after 1/rate")
	}
	if ok, _ := l.Allow("a", t0.Add(time.Hour)); !ok { // a long idle refills to burst, not beyond
		t.Fatal("idle bucket refused")
	}
	for range 2 {
		l.Allow("a", t0.Add(time.Hour))
	}
	if ok, _ := l.Allow("a", t0.Add(time.Hour)); ok {
		t.Fatal("bucket held more than burst after a long idle")
	}
}

// A clock going backwards must not mint tokens or panic.
func TestSourceLimiterClockBackwards(t *testing.T) {
	l := NewSourceLimiter(1, 1, 10)
	l.Allow("a", t0)
	if ok, wait := l.Allow("a", t0.Add(-time.Hour)); ok || wait <= 0 {
		t.Fatalf("backwards clock: %v %v", ok, wait)
	}
}

// With max sources held, new sources share the overflow bucket until idle buckets are swept (at most once per
// second), so memory stays bounded and a many-source flood is cut to one bucket.
func TestSourceLimiterBoundedSources(t *testing.T) {
	l := NewSourceLimiter(1, 2, 3)
	for i := range 3 {
		l.Allow(fmt.Sprint("s", i), t0)
	}
	// Map full, nothing idle: new sources share overflow (burst 2).
	got := 0
	for i := range 10 {
		if ok, _ := l.Allow(fmt.Sprint("new", i), t0); ok {
			got++
		}
	}
	if got != 2 || l.Sources() != 3 {
		t.Fatalf("overflow allowed %d (want 2), sources %d (want 3)", got, l.Sources())
	}
	// After the buckets refill (1 token in 1 s → full at burst 2 after 1 s since they hold 1), a sweep frees them.
	if ok, _ := l.Allow("late", t0.Add(2*time.Second)); !ok || l.Sources() != 1 {
		t.Fatalf("after idle: ok %v, sources %d; want the idle buckets swept and late given its own", ok, l.Sources())
	}
}

func TestSourceKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"192.0.2.7:4431", "192.0.2.7"},
		{"[2001:db8:1:2:3:4:5:6]:443", "2001:db8:1:2::/64"},
		{"[::ffff:192.0.2.7]:1", "192.0.2.7"},
		{"192.0.2.7", "192.0.2.7"},
		{"pipe", "pipe"},
	} {
		if got := SourceKey(tc.in); got != tc.want {
			t.Errorf("SourceKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
