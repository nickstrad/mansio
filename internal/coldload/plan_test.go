package coldload_test

import (
	"slices"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/coldload"
)

func normalized(t *testing.T, c coldload.Config) coldload.Config {
	t.Helper()
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLevels(t *testing.T) {
	c := normalized(t, coldload.Config{
		Name: "t", PerLevel: 2, Concurrency: []int{1, 4},
		Sizes: []coldload.SizeClass{
			{Name: "a", Size: "1KiB", Count: 3},                        // PerLevel < Count: windows wrap around
			{Name: "b", Size: "2KiB", Count: 1, Concurrency: []int{8}}, // PerLevel > Count: capped at 1
		},
		DupCallers: 5,
	})
	levels := c.Levels()
	type want struct {
		kind, class string
		conc        int
		keys        []int
	}
	wants := []want{
		{coldload.KindCold, "a", 1, []int{0, 1}},
		{coldload.KindCold, "a", 4, []int{2, 0}}, // cursor wrapped past Count
		{coldload.KindCold, "b", 8, []int{0}},
		{coldload.KindDup, "b", 5, []int{0, 0, 0, 0, 0}}, // last key of the last class, repeated
	}
	if len(levels) != len(wants) {
		t.Fatalf("got %d levels, want %d", len(levels), len(wants))
	}
	for i, w := range wants {
		l := levels[i]
		var keys []string
		for _, k := range w.keys {
			keys = append(keys, c.Key(l.Class, k))
		}
		if l.Index != i || l.Kind != w.kind || l.Class.Name != w.class || l.Conc != w.conc || !slices.Equal(l.Keys, keys) {
			t.Fatalf("level %d = %+v; want %+v (keys %v)", i, l, w, keys)
		}
	}
	if got := coldload.MaxConcurrency(levels); got != 5 { // b's c=8 is capped by its 1 key; dup has 5
		t.Fatalf("MaxConcurrency = %d, want 5", got)
	}
}

func TestLevelsWithoutDup(t *testing.T) {
	c := normalized(t, coldload.Config{Name: "t", Sizes: []coldload.SizeClass{{Size: "1KiB", Count: 1}}})
	if l := c.Levels(); len(l) != 1 || l[0].Kind != coldload.KindCold {
		t.Fatalf("levels = %+v", l)
	}
}

func TestKey(t *testing.T) {
	c := normalized(t, coldload.Config{Name: "t", Prefix: "p", Sizes: []coldload.SizeClass{{Name: "64k", Size: "64KiB", Count: 1}}})
	if got := c.Key(c.Sizes[0], 7); got != "p/64k/db-000007.sqlite" {
		t.Fatalf("Key = %q", got)
	}
}

func TestMissingAndToUpload(t *testing.T) {
	c := normalized(t, coldload.Config{Name: "t", Sizes: []coldload.SizeClass{{Name: "a", Size: "1KiB", Count: 3}}})
	s := c.Sizes[0]
	have := map[string]int64{c.Key(s, 0): 100, c.Key(s, 1): 99} // 0 right size, 1 wrong size, 2 missing
	if got := c.Missing(have); !slices.Equal(got, []string{c.Key(s, 2)}) {
		t.Fatalf("Missing = %v", got)
	}
	if got := c.ToUpload(s, 100, have); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("ToUpload = %v, want [1 2]", got)
	}
	if got := c.ToUpload(s, 100, map[string]int64{c.Key(s, 0): 100, c.Key(s, 1): 100, c.Key(s, 2): 100}); len(got) != 0 {
		t.Fatalf("ToUpload with everything present = %v", got)
	}
	if got := c.Missing(nil); len(got) != 3 {
		t.Fatalf("Missing(nil) = %v", got)
	}
}
