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

// TestClusterSeeds is DST-2: each seed is one simulated cluster run (agents, controld, the edge, clients) under
// network, storage and process faults, checked for split brain, lost or duplicated writes and
// linearizability. MANSIO_SIM_SWARM=1 varies the fault rates per seed (DST-3's swarm).
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
