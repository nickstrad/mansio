package load

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/schema"
	"github.com/nickstrad/mansio/server/internal/workload"
)

func TestWeighted(t *testing.T) {
	weights := []float64{1, 0, 3, -1} // only 0 and 2 can be picked
	w := newWeighted(len(weights), func(i int) float64 { return weights[i] })
	r := rand.New(rand.NewPCG(1, 1))
	counts := make([]int, len(weights))
	const n = 40_000
	for range n {
		counts[w.pick(r)]++
	}
	if counts[1] != 0 || counts[3] != 0 {
		t.Fatalf("picked a non-positive weight: %v", counts)
	}
	if got := float64(counts[2]) / n; math.Abs(got-0.75) > 0.01 {
		t.Fatalf("index 2 picked %.3f of the time, want 0.75", got)
	}
}

func newTestPicker(t *testing.T, body string) (*Picker, *Config) {
	t.Helper()
	c := parse(t, body)
	cat := schema.NewCatalog(c.Seed, c.Schema)
	cat.Ensure(c.MaxDBs())
	return NewPicker(rand.New(rand.NewPCG(1, 2)), NewGenerators(cat, c.Workload), c, &c.Phases[0]), c
}

func TestPickerHonorsThePhase(t *testing.T) {
	for _, dist := range []string{"uniform", "zipf"} {
		t.Run(dist, func(t *testing.T) {
			p, c := newTestPicker(t, `{"record_fraction":0.25,"distribution":{"kind":"`+dist+`"},
				"clients":[{"name":"a","share":1},{"name":"b","share":1},{"name":"c","share":1}],
				"phases":[{"dbs":7,"duration":"1s","mix":{"insert":1,"point_read":1},"client_shares":{"a":1,"c":1}}]}`)
			recorded, perDB := 0, make([]int, 7)
			for range 5000 {
				pk := p.Next()
				if pk.DB < 0 || pk.DB >= 7 {
					t.Fatalf("db %d outside the phase's 7", pk.DB)
				}
				perDB[pk.DB]++
				if pk.Class != workload.Insert && pk.Class != workload.PointRead {
					t.Fatalf("class %s is not in the phase mix", pk.Class)
				}
				if name := c.Clients[pk.Client].Name; name == "b" {
					t.Fatal("client b has no share in this phase")
				}
				if pk.Record {
					recorded++
				}
			}
			if recorded < 1100 || recorded > 1400 {
				t.Fatalf("recorded %d of 5000 at fraction 0.25", recorded)
			}
			if dist == "zipf" && perDB[0] < 3*perDB[6] {
				t.Fatalf("zipf should favor low ids: %v", perDB)
			}
		})
	}
}

func TestAssertionsCanFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out (-tags noassert)")
	}
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if p := recover(); p == nil || !strings.HasPrefix(fmt.Sprint(p), "assertion failed") {
				t.Errorf("%s: want an assertion failure, got %v", name, p)
			}
		}()
		f()
	}
	mustPanic("all-zero weights", func() { newWeighted(2, func(int) float64 { return 0 }) })
	mustPanic("zero-length phase", func() { NewPacer(&Phase{}, time.Now()) })
}
