package coldload_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/coldload"
	"github.com/nickstrad/mansio/server/internal/errs"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1", 1, false},                // on point: smallest valid size
		{"1B", 1, false},               // unit B
		{"0", 0, true},                 // off point below 1 byte
		{"0.5", 0, true},               // off point: rounds below one byte
		{"0.0001K", 0, true},           // a unit that still lands below one byte
		{"512", 512, false},            // no unit
		{"64KiB", 64 << 10, false},     // each unit spelling
		{"64kib", 64 << 10, false},     // lower case
		{" 64 KiB ", 64 << 10, false},  // spaces
		{"1.5MiB", 3 << 19, false},     // fraction
		{"16M", 16 << 20, false},       // short unit
		{"2GB", 2 << 30, false},        // KB/MB/GB are binary too
		{"1024GiB", 1 << 40, false},    // large, in range
		{"1048576GiB", 1 << 50, false}, // on point: 1 PiB
		{"1048577GiB", 0, true},        // off point: over 1 PiB
		{"", 0, true},                  // empty
		{"KiB", 0, true},               // unit without number
		{"abc", 0, true},               // not a number
		{"-1MiB", 0, true},             // negative
		{"Inf", 0, true},               // non-finite
		{"NaN", 0, true},               // non-finite
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := coldload.ParseSize(tc.in)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("ParseSize(%q) = %d, %v; want %d, err=%v", tc.in, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func valid() coldload.Config {
	return coldload.Config{Name: "t", Sizes: []coldload.SizeClass{{Name: "small", Size: "64KiB", Count: 3}}}
}

func TestNormalizeRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*coldload.Config)
		want   string
	}{
		{"no name", func(c *coldload.Config) { c.Name = "" }, "name is required"},
		{"no sizes", func(c *coldload.Config) { c.Sizes = nil }, "sizes is empty"},
		{"bad size", func(c *coldload.Config) { c.Sizes[0].Size = "lots" }, "bad size"},
		{"count 0 (off point)", func(c *coldload.Config) { c.Sizes[0].Count = 0 }, "count must be"},
		{"concurrency 0", func(c *coldload.Config) { c.Concurrency = []int{1, 0} }, "concurrency"},
		{"class concurrency 0", func(c *coldload.Config) { c.Sizes[0].Concurrency = []int{0} }, "concurrency"},
		{"duplicate class names", func(c *coldload.Config) { c.Sizes = append(c.Sizes, c.Sizes[0]) }, "duplicate name"},
		{"negative per_level", func(c *coldload.Config) { c.PerLevel = -1 }, "must not be negative"},
		{"negative max_fetches", func(c *coldload.Config) { c.MaxFetches = -1 }, "must not be negative"},
		{"negative max_fetch_queue", func(c *coldload.Config) { c.MaxFetchQueue = -1 }, "must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c)
			err := c.Normalize()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Normalize err = %v, want one containing %q", err, tc.want)
			}
			if e, ok := errs.Outermost(err); !ok || e.Module != "coldload" {
				t.Fatalf("err is not a well-formed coldload error: %T", err)
			}
		})
	}
}

func TestNormalizeDefaults(t *testing.T) {
	c := valid()
	c.Sizes = append(c.Sizes, coldload.SizeClass{Size: "1MiB", Count: 1 /* on point */, Concurrency: []int{1, 4}})
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	s0, s1 := c.Sizes[0], c.Sizes[1]
	switch {
	case s0.Bytes != 64<<10 || s1.Bytes != 1<<20:
		t.Fatalf("Bytes = %d, %d", s0.Bytes, s1.Bytes)
	case s1.Name != "1mib":
		t.Fatalf("default class name = %q", s1.Name)
	case len(s0.Concurrency) != 1 || s0.Concurrency[0] != 1:
		t.Fatalf("class inherits default concurrency [1], got %v", s0.Concurrency)
	case len(s1.Concurrency) != 2:
		t.Fatalf("class override lost: %v", s1.Concurrency)
	case c.S3.Endpoint != "localhost:8333" || c.S3.Bucket != "coldload" || c.Prefix != "dbs":
		t.Fatalf("S3 defaults: %+v prefix %q", c.S3, c.Prefix)
	case c.PerLevel != 100 || c.QueryTimeout.Duration != 10*time.Minute || c.UploadWorkers != 16 ||
		c.ActivationTimeout.Duration != 10*time.Minute || c.MaxFetches != 0 || c.MaxFetchQueue != 0:
		t.Fatalf("numeric defaults: %d %v %d %v %d %d", c.PerLevel, c.QueryTimeout, c.UploadWorkers, c.ActivationTimeout, c.MaxFetches, c.MaxFetchQueue)
	case c.OutDir != "results/coldload/t" || c.CacheDir != "results/coldload/t/cache":
		t.Fatalf("dirs: %q %q", c.OutDir, c.CacheDir)
	}
}

func TestDurationJSON(t *testing.T) {
	var d coldload.Duration
	if err := d.UnmarshalJSON([]byte(`"90s"`)); err != nil || d.Duration != 90*time.Second {
		t.Fatalf("got %v, %v", d, err)
	}
	for _, bad := range []string{`90`, `"ninety"`} {
		if err := d.UnmarshalJSON([]byte(bad)); err == nil {
			t.Fatalf("UnmarshalJSON(%s) accepted", bad)
		}
	}
	if b, _ := (coldload.Duration{Duration: time.Minute}).MarshalJSON(); string(b) != `"1m0s"` {
		t.Fatalf("MarshalJSON = %s", b)
	}
}

func TestHuman(t *testing.T) {
	for in, want := range map[int64]string{1023: "1023 B", 1024: "1 KiB", 3 << 19: "1.5 MiB", 1 << 30: "1.00 GiB"} {
		if got := coldload.Human(in); got != want {
			t.Errorf("Human(%d) = %q, want %q", in, got, want)
		}
	}
}
