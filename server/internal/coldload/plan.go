package coldload

import (
	"fmt"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// Level kinds.
const (
	KindCold = "cold" // distinct cold databases
	KindDup  = "dup"  // many callers for one cold database (one activation expected)
)

// Level is one measured step: activate Keys with Conc in flight, then passivate them all.
type Level struct {
	Index int
	Kind  string
	Class SizeClass
	Conc  int
	Keys  []string // may repeat (dup level)
}

// Key is the object key of database i of class s.
func (c *Config) Key(s SizeClass, i int) string {
	return fmt.Sprintf("%s/%s/db-%06d.sqlite", c.Prefix, s.Name, i)
}

// Levels plans the run: for each class, one cold level per concurrency value, each taking the next
// min(PerLevel, Count) databases of the class (wrapping around); then, if DupCallers > 0, one dup level of
// DupCallers callers for the last database of the last class.
//
//	pre:  c was normalized (Normalize returned nil)
//	post: indexes are 0..n-1 in order; every level has ≥ 1 key and Conc ≥ 1
func (c *Config) Levels() []Level {
	var out []Level
	for _, s := range c.Sizes {
		assert.That(s.Count >= 1 && len(s.Concurrency) > 0, "Levels: class %s not normalized", s.Name)
		n := min(c.PerLevel, s.Count)
		cursor := 0
		for _, conc := range s.Concurrency {
			keys := make([]string, n)
			for i := range keys {
				keys[i] = c.Key(s, (cursor+i)%s.Count)
			}
			cursor = (cursor + n) % s.Count
			out = append(out, Level{Index: len(out), Kind: KindCold, Class: s, Conc: conc, Keys: keys})
		}
	}
	if c.DupCallers > 0 {
		s := c.Sizes[len(c.Sizes)-1]
		keys := make([]string, c.DupCallers)
		for i := range keys {
			keys[i] = c.Key(s, s.Count-1)
		}
		out = append(out, Level{Index: len(out), Kind: KindDup, Class: s, Conc: c.DupCallers, Keys: keys})
	}
	if assert.Enabled {
		for i, l := range out {
			assert.That(l.Index == i && len(l.Keys) > 0 && l.Conc >= 1, "Levels: bad level %d: %+v", i, l)
		}
	}
	return out
}

// MaxConcurrency is the most activations any level runs at once.
func MaxConcurrency(levels []Level) int {
	m := 0
	for _, l := range levels {
		m = max(m, min(l.Conc, len(l.Keys)))
	}
	return m
}

// Missing lists the configured objects that are not in have (key → size).
func (c *Config) Missing(have map[string]int64) []string {
	var out []string
	for _, s := range c.Sizes {
		for i := range s.Count {
			if _, ok := have[c.Key(s, i)]; !ok {
				out = append(out, c.Key(s, i))
			}
		}
	}
	return out
}

// ToUpload lists the indexes of class s whose object is missing or not exactly size bytes.
func (c *Config) ToUpload(s SizeClass, size int64, have map[string]int64) []int {
	var out []int
	for i := range s.Count {
		if got, ok := have[c.Key(s, i)]; !ok || got != size {
			out = append(out, i)
		}
	}
	return out
}
