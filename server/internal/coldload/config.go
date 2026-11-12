// Package coldload is the domain of the coldload benchmark: what to measure and how to summarise it. It does
// no I/O. Config validation, the level plan, the level runner (through the Getter port) and the CSV row
// layout live here; cmd/bench/coldload wires them to S3, SQLite and files.
//
// Errors leaving the package are errs.Error with module "coldload".
package coldload

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
)

const module = "coldload"

// Config is one experiment (configs/coldload/*.json).
type Config struct {
	Name string `json:"name"`

	S3     S3Config `json:"s3"`
	Prefix string   `json:"prefix"` // object keys: <prefix>/<size class>/db-000000.sqlite

	Sizes []SizeClass `json:"sizes"`

	Concurrency  []int    `json:"concurrency"`  // activations in flight at once; one level per value
	PerLevel     int      `json:"per_level"`    // activations per (size, concurrency); capped by the class count
	FirstQuery   string   `json:"first_query"`  // "ready" = this answered
	WarmQueries  []string `json:"warm_queries"` // run after first_query, before ready
	KeepLocal    bool     `json:"keep_local"`   // keep local copies between levels (later levels are local hits)
	DupCallers   int      `json:"dup_callers"`  // callers asking for one cold database at once; 0 = no dup level
	ReadConns    int      `json:"read_conns"`   // actor read pool per database
	CacheKiB     int      `json:"cache_kib"`    // SQLite page cache per connection
	QueryTimeout Duration `json:"query_timeout"`
	// ActivationTimeout bounds one whole activation (fetch + open + queries); default 10m, so the large classes
	// are measured rather than cut off. The vactor host's own default is 30 s.
	ActivationTimeout Duration `json:"activation_timeout"`
	// MaxFetches and MaxFetchQueue are the host's fetch bulkhead (vactor.Options); 0 = vactor's defaults (16
	// in flight, 256 queued). Concurrency levels above max_fetches measure queueing behind the bulkhead.
	MaxFetches    int `json:"max_fetches"`
	MaxFetchQueue int `json:"max_fetch_queue"`

	UploadWorkers int    `json:"upload_workers"`
	WorkDir       string `json:"work_dir"`  // templates; default results/coldload/templates
	OutDir        string `json:"out_dir"`   // default results/coldload/<name>
	CacheDir      string `json:"cache_dir"` // local copies; default <out_dir>/cache
}

// S3Config says where the objects live. Defaults match deploy/seaweedfs.
type S3Config struct {
	Endpoint  string `json:"endpoint"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Bucket    string `json:"bucket"`
}

// SizeClass is a group of databases of about the same size.
type SizeClass struct {
	Name  string `json:"name"`
	Size  string `json:"size"` // e.g. "64KiB", "16MiB", "1GiB"
	Count int    `json:"count"`
	// Concurrency overrides Config.Concurrency for this class (e.g. fewer levels for huge files).
	Concurrency []int `json:"concurrency,omitempty"`

	Bytes int64 `json:"-"` // parsed from Size by Normalize
}

// Duration is a time.Duration that reads from JSON as a string such as "30s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\", got %s", b)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(d.String())), nil }

// Normalize validates c and fills in defaults. It is the only validation: everything after it trusts c.
//
//	post: err == nil ⇒ every class has Bytes ≥ 1, Count ≥ 1 and a non-empty Concurrency with values ≥ 1
func (c *Config) Normalize() error {
	bad := func(format string, args ...any) error {
		return errs.Wrap(module, nil, "invalid config %q: "+format, append([]any{c.Name}, args...)...)
	}
	if c.Name == "" {
		return errs.Wrap(module, nil, "invalid config: name is required")
	}
	if len(c.Sizes) == 0 {
		return bad("sizes is empty")
	}
	if len(c.Concurrency) == 0 {
		c.Concurrency = []int{1}
	}
	if err := checkConcurrency(c.Concurrency); err != nil {
		return bad("concurrency: %v", err)
	}
	seen := map[string]bool{}
	for i := range c.Sizes {
		s := &c.Sizes[i]
		n, err := ParseSize(s.Size)
		if err != nil {
			return bad("sizes[%d]: %v", i, err)
		}
		s.Bytes = n
		if s.Name == "" {
			s.Name = strings.ToLower(s.Size)
		}
		if seen[s.Name] {
			return bad("sizes[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		if s.Count < 1 {
			return bad("sizes[%d] %s: count must be ≥ 1, got %d", i, s.Name, s.Count)
		}
		if len(s.Concurrency) == 0 {
			s.Concurrency = c.Concurrency
		}
		if err := checkConcurrency(s.Concurrency); err != nil {
			return bad("sizes[%d] %s concurrency: %v", i, s.Name, err)
		}
	}
	if c.PerLevel < 0 || c.DupCallers < 0 || c.ReadConns < 0 || c.CacheKiB < 0 || c.UploadWorkers < 0 || c.MaxFetches < 0 || c.MaxFetchQueue < 0 {
		return bad("per_level, dup_callers, read_conns, cache_kib, upload_workers, max_fetches and max_fetch_queue must not be negative")
	}
	setDefault(&c.S3.Endpoint, "localhost:8333")
	setDefault(&c.S3.AccessKey, "coldload")
	setDefault(&c.S3.SecretKey, "coldload-secret")
	setDefault(&c.S3.Bucket, "coldload")
	setDefault(&c.Prefix, "dbs")
	setDefault(&c.FirstQuery, "SELECT k, name FROM items WHERE id = 1")
	setDefault(&c.WorkDir, "results/coldload/templates")
	setDefault(&c.OutDir, "results/coldload/"+c.Name)
	setDefault(&c.CacheDir, c.OutDir+"/cache")
	if c.PerLevel == 0 {
		c.PerLevel = 100
	}
	if c.QueryTimeout.Duration <= 0 {
		c.QueryTimeout.Duration = 10 * time.Minute
	}
	if c.ActivationTimeout.Duration <= 0 {
		c.ActivationTimeout.Duration = 10 * time.Minute
	}
	if c.UploadWorkers == 0 {
		c.UploadWorkers = 16
	}
	return nil
}

func setDefault(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func checkConcurrency(cs []int) error {
	for _, c := range cs {
		if c < 1 {
			return fmt.Errorf("values must be ≥ 1, got %d", c)
		}
	}
	return nil
}

// maxBytes bounds sizes to 1 PiB so the int64 conversion cannot overflow.
const maxBytes = 1 << 50

// ParseSize reads "512", "1.5MiB", "64KiB", "1GiB" (also K/M/G and KB/MB/GB, all binary units). The
// result is at least one byte.
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := 1.0
	for _, u := range []struct {
		suffix string
		m      float64
	}{{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.m
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("bad size %q: want a number with an optional unit such as 64KiB", s)
	}
	n := v * mult
	if n < 1 || n > maxBytes {
		return 0, fmt.Errorf("bad size %q: must be between 1 byte and 1 PiB", s)
	}
	return int64(n), nil
}

// Human prints a byte count with a binary unit, e.g. "1.5 MiB".
func Human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}
