package stats

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// CSV writes a low-volume file (phases, samples, schemas) a row at a time, flushing each row so the file can be
// read mid-run. encoding/csv quotes fields as needed. Safe for concurrent use.
type CSV struct {
	mu sync.Mutex
	f  *os.File
	w  *csv.Writer
}

// NewCSV creates path and writes the header.
func NewCSV(path string, header ...string) (*CSV, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, errs.Wrap("stats", err, "cannot create %s", path)
	}
	c := &CSV{f: f, w: csv.NewWriter(f)}
	return c, c.write(header)
}

// Row writes one row; see Field for how values are formatted.
func (c *CSV) Row(values ...any) error {
	fields := make([]string, len(values))
	for i, v := range values {
		fields[i] = Field(v)
	}
	return c.write(fields)
}

func (c *CSV) write(fields []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w.Write(fields)
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		return errs.Wrap("stats", err, "cannot write %s", c.f.Name())
	}
	return nil
}

// Close closes the file.
func (c *CSV) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Close()
}

// Field formats one value: floats compactly, durations as milliseconds, everything else with fmt.
func Field(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case time.Duration:
		return strconv.FormatFloat(float64(x.Microseconds())/1e3, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}
