package objstore

import (
	"testing"
	"time"
)

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
	if c.MaxRetries != 1 || c.OpTimeout != 5*time.Second || c.StallTimeout != 10*time.Second ||
		c.MinThroughput != 32<<20 || c.MaxConns != 1024 {
		t.Fatalf("defaults: %+v", c)
	}
	c = Config{MaxRetries: 3, OpTimeout: time.Second}
	c.normalize()
	if c.MaxRetries != 3 || c.OpTimeout != time.Second {
		t.Fatalf("explicit values overwritten: %+v", c)
	}
}
