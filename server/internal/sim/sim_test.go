package sim_test

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/nickstrad/mansio/server/internal/sim"
)

// Seeds to run: MANSIO_SIM_SEED=s runs that one seed; MANSIO_SIM_SEEDS=n runs n seeds from
// MANSIO_SIM_FIRST (default 1). Default: defaultSeeds (8; 3 under -race), so `go test ./...` stays quick.
func seeds(t *testing.T) []uint64 {
	if s := os.Getenv("MANSIO_SIM_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("MANSIO_SIM_SEED=%q: %v", s, err)
		}
		return []uint64{v}
	}
	n, first := uint64(defaultSeeds), uint64(1)
	for name, p := range map[string]*uint64{"MANSIO_SIM_SEEDS": &n, "MANSIO_SIM_FIRST": &first} {
		if s := os.Getenv(name); s != "" {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				t.Fatalf("%s=%q: %v", name, s, err)
			}
			*p = v
		}
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = first + uint64(i)
	}
	return out
}

// run runs cfg in its own synctest bubble. MANSIO_SIM_TRACE=1 prints the trace as it goes.
func run(t *testing.T, cfg sim.Config) sim.Result {
	t.Helper()
	cfg.Dir = t.TempDir()
	cfg.KeepGoing = cfg.KeepGoing || os.Getenv("MANSIO_SIM_KEEPGOING") != ""
	var live io.Writer
	if os.Getenv("MANSIO_SIM_TRACE") != "" {
		live = os.Stdout
	}
	var res sim.Result
	synctest.Test(t, func(t *testing.T) { res = sim.Run(cfg, live) })
	return res
}

func replay(seed uint64) string {
	return fmt.Sprintf("MANSIO_SIM_SEED=%d MANSIO_SIM_TRACE=1 go test ./internal/sim -run 'TestSeeds' -v", seed)
}

// TestSeeds is DST-1: each seed is one simulated run of the single-agent request path under faults and
// crashes, checked for lost, duplicated or phantom writes (see package sim).
func TestSeeds(t *testing.T) {
	for _, seed := range seeds(t) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			res := run(t, sim.Config{Seed: seed})
			if res.Err == nil && len(res.Violations) == 0 {
				t.Logf("seed %d: %+v", seed, res.Stats)
				return
			}
			var b strings.Builder
			fmt.Fprintf(&b, "seed %d failed (%s)\n", seed, res.Settings)
			if res.Err != nil {
				fmt.Fprintf(&b, "  run error: %v\n", res.Err)
			}
			for _, v := range res.Violations {
				fmt.Fprintf(&b, "  %s\n", v)
			}
			tail := res.Trace[max(0, len(res.Trace)-40):]
			fmt.Fprintf(&b, "last %d trace lines:\n  %s\nreplay: %s\n", len(tail), strings.Join(tail, "\n  "), replay(seed))
			t.Error(b.String())
		})
	}
}

// The same seed gives a byte-identical trace: every run of it, under -race and -count=N too.
func TestSameSeedSameTrace(t *testing.T) {
	for seed := range uint64(determinismSeeds) {
		seed++
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			a, b := run(t, sim.Config{Seed: seed}), run(t, sim.Config{Seed: seed})
			if a.Hash == b.Hash {
				if len(a.Trace) < 50 {
					t.Fatalf("seed %d: only %d trace lines; the run did nothing worth comparing", seed, len(a.Trace))
				}
				return
			}
			for i := range min(len(a.Trace), len(b.Trace)) {
				if a.Trace[i] != b.Trace[i] {
					t.Fatalf("seed %d: traces differ at line %d:\n  %s\n  %s", seed, i, a.Trace[i], b.Trace[i])
				}
			}
			t.Fatalf("seed %d: traces differ in length: %d vs %d lines", seed, len(a.Trace), len(b.Trace))
		})
	}
}

// MANSIO_SIM_SHRINK=s shrinks failing seed s (sim.Shrink) and prints the smallest failing run's knobs.
func TestShrink(t *testing.T) {
	s := os.Getenv("MANSIO_SIM_SHRINK")
	if s == "" {
		t.Skip("set MANSIO_SIM_SHRINK=<seed> to shrink a failing seed")
	}
	seed, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("MANSIO_SIM_SHRINK=%q: %v", s, err)
	}
	cfg := sim.Config{Seed: seed}
	res := run(t, cfg)
	if res.Err == nil && len(res.Violations) == 0 {
		t.Fatalf("seed %d passes: nothing to shrink", seed)
	}
	runs := 1
	_, small := sim.Shrink(cfg, res, func(c sim.Config) sim.Result { runs++; return run(t, c) })
	t.Logf("seed %d shrunk in %d runs to: %s", seed, runs, small.Settings)
	for _, v := range small.Violations {
		t.Logf("  %s", v)
	}
}
