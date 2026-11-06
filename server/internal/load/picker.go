package load

import (
	"math/rand/v2"
	"slices"

	"github.com/nickstrad/mansio/server/internal/assert"
	"github.com/nickstrad/mansio/server/internal/schema"
	"github.com/nickstrad/mansio/server/internal/workload"
)

// weighted picks an index in [0, n) with probability proportional to its weight; indexes with a weight <= 0
// are never picked.
//
//	Invariant: len(idx) == len(cum) > 0 and cum is strictly increasing.
type weighted struct {
	idx []int
	cum []float64
}

// newWeighted builds the picker for weights weight(0..n-1).
//
//	pre: some weight is positive
func newWeighted(n int, weight func(int) float64) weighted {
	var w weighted
	total := 0.0
	for i := range n {
		if x := weight(i); x > 0 {
			total += x
			w.idx = append(w.idx, i)
			w.cum = append(w.cum, total)
		}
	}
	assert.That(len(w.idx) > 0, "newWeighted: no positive weight among %d", n)
	return w
}

func (w weighted) pick(r *rand.Rand) int {
	i, _ := slices.BinarySearch(w.cum, r.Float64()*w.cum[len(w.cum)-1])
	return w.idx[min(i, len(w.idx)-1)]
}

// Generators keeps one workload generator per database, so id tracking survives across phases. Not safe for
// concurrent use.
type Generators struct {
	catalog *schema.Catalog
	opts    workload.Options
	gens    []*workload.Generator
}

func NewGenerators(c *schema.Catalog, opts workload.Options) *Generators {
	return &Generators{catalog: c, opts: opts}
}

// Get returns database db's generator, creating it on first use.
//
//	pre: db's schema is in the catalog
func (g *Generators) Get(db int) *workload.Generator {
	if db >= len(g.gens) {
		g.gens = append(g.gens, make([]*workload.Generator, db+1-len(g.gens))...)
	}
	if g.gens[db] == nil {
		g.gens[db] = workload.NewGenerator(g.catalog.Get(db), g.opts)
	}
	return g.gens[db]
}

// Pick is one generated request: which database, which client profile sends it, whether it is recorded, and the
// statement.
type Pick struct {
	DB     int
	Client int // index into Config.Clients
	Record bool
	workload.Statement
}

// Picker chooses each request's database, class and client, and generates its SQL. Not safe for concurrent use:
// the dispatcher owns one per phase.
type Picker struct {
	rng      *rand.Rand
	gens     *Generators
	classes  weighted // index into workload.Classes
	clients  weighted // index into Config.Clients
	dbs      int
	zipf     *rand.Zipf
	fraction float64
}

// NewPicker builds the picker for one phase.
//
//	pre: c is normalized (so the phase's mix and client shares each have a positive weight)
func NewPicker(rng *rand.Rand, gens *Generators, c *Config, phase *Phase) *Picker {
	p := &Picker{rng: rng, gens: gens, dbs: phase.DBs, fraction: c.RecordFraction,
		classes: newWeighted(len(workload.Classes), func(i int) float64 { return phase.Mix[workload.Classes[i]] }),
		clients: newWeighted(len(c.Clients), func(i int) float64 { return phase.ClientShares[c.Clients[i].Name] }),
	}
	if phase.Distribution.Kind == Zipf && phase.DBs > 1 {
		p.zipf = rand.NewZipf(rng, phase.Distribution.S, 1, uint64(phase.DBs-1))
	}
	return p
}

// Next generates one request.
//
//	post: 0 <= DB < phase.DBs
func (p *Picker) Next() Pick {
	var db int
	if p.zipf != nil {
		db = int(p.zipf.Uint64())
	} else {
		db = p.rng.IntN(p.dbs)
	}
	st := p.gens.Get(db).Next(p.rng, workload.Classes[p.classes.pick(p.rng)])
	return Pick{DB: db, Client: p.clients.pick(p.rng), Record: p.rng.Float64() < p.fraction, Statement: st}
}
