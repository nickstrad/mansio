package objstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"
)

// A transport timeout (net/http's ResponseHeaderTimeout matches context.DeadlineExceeded) while the caller
// still waits is our timeout: Transient, retried. The caller's own deadline stays Canceled.
func TestAttemptEndMarksOurTimeouts(t *testing.T) {
	transport := &url.Error{Op: "Get", URL: "http://s3/bkt/?location=", Err: fmt.Errorf("timeout awaiting response headers: %w", context.DeadlineExceeded)}
	if k := Classify(transport); k != Canceled {
		t.Fatalf("precondition: Classify(transport timeout) = %v, want canceled (why end must mark it)", k)
	}
	a := begin(context.Background(), time.Hour)
	if err := a.end(transport); Classify(err) != Transient || !errors.Is(err, ErrTimeout) {
		t.Fatalf("transport timeout with the caller waiting: %v (%v), want Transient ErrTimeout", err, Classify(err))
	}

	a = begin(context.Background(), time.Hour)
	if err := a.end(errors.New("boom")); err.Error() != "boom" {
		t.Fatalf("other errors pass through: %v", err)
	}

	parent, cancel := context.WithCancel(context.Background())
	a = begin(parent, time.Hour)
	cancel()
	if err := a.end(transport); Classify(err) != Canceled {
		t.Fatalf("caller gone: %v (%v), want Canceled", err, Classify(err))
	}

	a = begin(context.Background(), time.Millisecond)
	<-a.ctx.Done()
	if err := a.end(errors.New("read: use of closed connection")); !errors.Is(err, ErrTimeout) {
		t.Fatalf("own deadline: %v, want ErrTimeout", err)
	}
}

func TestTransferTime(t *testing.T) {
	s := &Store{cfg: Config{OpTimeout: 5 * time.Second, MinThroughput: 32 << 20}}
	for _, tc := range []struct {
		size int64
		want time.Duration
	}{
		{-1, 5 * time.Second}, // unknown size: the op timeout only
		{0, 5 * time.Second},
		{1, 5*time.Second + time.Second/(32<<20)},
		{32 << 20, 6 * time.Second},
		{5 << 30, 5*time.Second + 160*time.Second},
		{1 << 50, 5*time.Second + (1<<50)/(32<<20)*time.Second}, // no overflow
	} {
		got := s.transferTime(tc.size)
		if d := got - tc.want; d < -time.Microsecond || d > time.Microsecond {
			t.Errorf("transferTime(%d) = %v, want %v", tc.size, got, tc.want)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{}
	c.normalize()
	if c.Logger == nil || c.OpTimeout != 5*time.Second || c.StallTimeout != 10*time.Second ||
		c.MinThroughput != 32<<20 || c.MaxConns != 1024 {
		t.Fatalf("defaults: %+v", c)
	}
	c = Config{OpTimeout: time.Second}
	c.normalize()
	if c.OpTimeout != time.Second {
		t.Fatalf("explicit values overwritten: %+v", c)
	}
}
