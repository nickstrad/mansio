package sim_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/nickstrad/mansio/server/internal/sim"
)

// runCluster runs cfg in its own synctest bubble. MANSIO_SIM_TRACE=1 prints the trace as it goes.
func runCluster(t *testing.T, cfg sim.ClusterConfig) sim.ClusterResult {
	t.Helper()
	cfg.Dir = t.TempDir()
	cfg.KeepGoing = cfg.KeepGoing || os.Getenv("MANSIO_SIM_KEEPGOING") != ""
	var live io.Writer
	if os.Getenv("MANSIO_SIM_TRACE") != "" {
		live = os.Stdout
	}
	var res sim.ClusterResult
	synctest.Test(t, func(t *testing.T) { res = sim.RunCluster(cfg, live) })
	return res
}

func clusterFailed(r sim.ClusterResult) bool { return r.Err != nil || len(r.Violations) > 0 }

func reportCluster(t *testing.T, res sim.ClusterResult, replay string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", res.Settings)
	if res.Err != nil {
		fmt.Fprintf(&b, "  run error: %v\n", res.Err)
	}
	for _, v := range res.Violations {
		fmt.Fprintf(&b, "  %s\n", v)
	}
	tail := res.Trace[max(0, len(res.Trace)-60):]
	fmt.Fprintf(&b, "last %d trace lines:\n  %s\nreplay: %s\n", len(tail), strings.Join(tail, "\n  "), replay)
	t.Error(b.String())
}

func clusterReplay(seed uint64, swarm bool) string {
	env := ""
	if swarm {
		env = "MANSIO_SIM_SWARM=1 "
	}
	return fmt.Sprintf("%sMANSIO_SIM_SEED=%d MANSIO_SIM_TRACE=1 go test ./internal/sim -run 'TestClusterSeeds' -v", env, seed)
}

// TestClusterSeeds runs the cluster simulation: each seed is one simulated cluster run (agents, controld, the edge,
// clients) under network, storage and process faults, checked for split brain, lost or duplicated writes and
// linearizability. MANSIO_SIM_SWARM=1 varies the fault rates per seed (swarm mode).
func TestClusterSeeds(t *testing.T) {
	swarm := os.Getenv("MANSIO_SIM_SWARM") != ""
	for _, seed := range seedsN(t, defaultClusterSeeds) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			res := runCluster(t, sim.ClusterConfig{Seed: seed, Swarm: swarm})
			if !clusterFailed(res) {
				t.Logf("seed %d: %+v", seed, res.Stats)
				return
			}
			reportCluster(t, res, clusterReplay(seed, swarm))
		})
	}
}

// The same seed gives a byte-identical cluster trace (MANSIO_SIM_SEEDS/FIRST/SWARM select the seeds here too).
func TestClusterSameSeedSameTrace(t *testing.T) {
	swarm := os.Getenv("MANSIO_SIM_SWARM") != ""
	for _, seed := range seedsN(t, clusterDeterminismSeeds) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			cfg := sim.ClusterConfig{Seed: seed, Swarm: swarm}
			a, b := runCluster(t, cfg), runCluster(t, cfg)
			if a.Hash == b.Hash {
				if len(a.Trace) < 100 {
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

// MANSIO_SIM_SHRINK=s shrinks failing cluster seed s (sim.ShrinkCluster; MANSIO_SIM_SWARM as for the run) and
// prints the smallest config that fails in the same class. MANSIO_SIM_SAVE=<dir> writes it as
// <dir>/seed-<s>.json (MANSIO_SIM_SAVE=1: testdata/open, an open finding with its expected class); once the bug
// is fixed, move it to testdata/cluster by hand, where it must pass.
func TestClusterShrink(t *testing.T) {
	s := os.Getenv("MANSIO_SIM_SHRINK")
	if s == "" {
		t.Skip("set MANSIO_SIM_SHRINK=<seed> to shrink a failing cluster seed")
	}
	seed, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("MANSIO_SIM_SHRINK=%q: %v", s, err)
	}
	cfg := sim.ClusterConfig{Seed: seed, Swarm: os.Getenv("MANSIO_SIM_SWARM") != ""}
	res := runCluster(t, cfg)
	if !clusterFailed(res) {
		t.Fatalf("seed %d passes: nothing to shrink", seed)
	}
	runs := 1
	small, sres := sim.ShrinkCluster(cfg, res, func(c sim.ClusterConfig) sim.ClusterResult { runs++; return runCluster(t, c) },
		func(s string) { t.Log(s) })
	t.Logf("seed %d shrunk in %d runs to: %s", seed, runs, sres.Settings)
	for _, op := range small.Script {
		t.Logf("  op %s", op)
	}
	for _, v := range sres.Violations {
		t.Logf("  %s", v)
	}
	if dir := os.Getenv("MANSIO_SIM_SAVE"); dir != "" {
		if dir == "1" {
			dir = "testdata/open" // replayed only with MANSIO_SIM_OPEN=1, so plain go test stays green
		}
		path := filepath.Join(dir, fmt.Sprintf("seed-%d.json", seed))
		why := ""
		if len(sres.Violations) > 0 {
			why = "OPEN: " + sres.Violations[0].What
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		v := sim.Vector{Name: fmt.Sprintf("seed-%d", seed), Why: why, Expect: sim.FailureClass(sres), Config: small}
		if err := sim.WriteVector(path, v); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s: once the bug is fixed, move it to testdata/cluster (drop its expect, edit its why)", path)
	}
}

// TestClusterVectors replays every committed regression vector (shrunk failures since fixed): each must pass.
func TestClusterVectors(t *testing.T) {
	paths, _ := filepath.Glob("testdata/cluster/*.json")
	for _, path := range paths {
		v, err := sim.ReadVector(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()
			if res := runCluster(t, v.Config); clusterFailed(res) {
				reportCluster(t, res, fmt.Sprintf("go test ./internal/sim -run 'TestClusterVectors/%s' -v  (%s)", v.Name, v.Why))
			}
		})
	}
}

// TestClusterOpenFindings (MANSIO_SIM_OPEN=1) replays the shrunk reproducers of findings not fixed yet
// (testdata/open, each with its why): each must still fail; once one passes, its fix landed: move it to
// testdata/cluster.
func TestClusterOpenFindings(t *testing.T) {
	if os.Getenv("MANSIO_SIM_OPEN") == "" {
		t.Skip("set MANSIO_SIM_OPEN=1 to replay the open findings (testdata/open)")
	}
	paths, _ := filepath.Glob("testdata/open/*.json")
	for _, path := range paths {
		v, err := sim.ReadVector(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(v.Name, func(t *testing.T) {
			res := runCluster(t, v.Config)
			if !clusterFailed(res) {
				t.Errorf("%s passes now: move it to testdata/cluster (and update its why)", path)
				return
			}
			if got := sim.FailureClass(res); v.Expect != "" && got != v.Expect {
				t.Errorf("%s fails as %s now, want %s:\n%v", path, got, v.Expect, res.Violations)
			}
			for _, x := range res.Violations {
				t.Logf("still open: %s", x)
			}
		})
	}
}
