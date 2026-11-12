package pg

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

// PG-04: the placement concurrency gate (SQL G-22: many workers on the same key).

func conns64(c *Config) { c.MaxConns = 64 }

// 64 goroutines CAS the same key from the same expected row: exactly one wins, 63 get ErrConflict, and the
// epoch went up by exactly one. Repeated so the race is not won by luck.
func TestConcurrentCASOneKey(t *testing.T) {
	p, _ := newPlacement(t, conns64)
	const workers, rounds = 64, 20
	vms := ownertest.SuiteVMs()
	cur := owner.Place{Owner: vms[0], Epoch: 1, State: owner.PlaceActive}
	must(t, p.CAS(ctx, "k", owner.Place{}, cur))
	for round := range rounds {
		start := make(chan struct{})
		errc := make(chan error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			next := owner.Place{Owner: vms[i%len(vms)], Epoch: cur.Epoch + 1, Gen: owner.Generation(i), State: owner.PlaceActive}
			wg.Go(func() { <-start; errc <- p.CAS(ctx, "k", cur, next) })
		}
		close(start)
		wg.Wait()
		close(errc)
		won, lost := 0, 0
		for err := range errc {
			switch {
			case err == nil:
				won++
			case errors.Is(err, owner.ErrConflict):
				lost++
			default:
				t.Fatalf("round %d: CAS = %v, want nil or ErrConflict", round, err)
			}
		}
		row, err := p.Get(ctx, "k")
		must(t, err)
		if won != 1 || lost != workers-1 || row.Epoch != cur.Epoch+1 {
			t.Fatalf("round %d: %d won, %d conflicted, epoch %d → %d; want 1, %d, +1", round, won, lost, cur.Epoch, row.Epoch, workers-1)
		}
		cur = row
	}
}

// moveStep is one step of the move protocol on key as a racing controller would take it: read, then CAS
// ACTIVE → MOVING (begin) or MOVING → ACTIVE on another VM at epoch + 1 (flip). It reports whether this
// call flipped.
func moveStep(p *Placement, key string, rng *rand.Rand) (flipped bool, err error) {
	row, err := p.Get(ctx, key)
	if err != nil {
		return false, err
	}
	next := row
	if row.State == owner.PlaceActive {
		next.State = owner.PlaceMoving
	} else {
		vms := ownertest.SuiteVMs()
		to := vms[rng.IntN(len(vms))]
		for to == row.Owner {
			to = vms[rng.IntN(len(vms))]
		}
		next = owner.Place{Owner: to, Epoch: row.Epoch + 1, Gen: row.Gen + owner.Generation(rng.IntN(3)), State: owner.PlaceActive}
	}
	err = p.CAS(ctx, key, row, next)
	if errors.Is(err, owner.ErrConflict) {
		return false, nil // someone else moved it: an answer
	}
	return err == nil && next.State == owner.PlaceActive, err
}

// 64 goroutines race moves over 16 keys. Afterwards the invariants query is empty, and for every key the
// moves rows are exactly the flips that succeeded: one per epoch step, from_epoch 1 … epoch-1.
func TestMoveRaceManyKeys(t *testing.T) {
	p, schema := newPlacement(t, conns64)
	const workers, keys, steps = 64, 16, 40
	vms := ownertest.SuiteVMs()
	for k := range keys {
		must(t, p.CAS(ctx, fmt.Sprintf("k%02d", k), owner.Place{}, owner.Place{Owner: vms[0], Epoch: 1, State: owner.PlaceActive}))
	}
	flips := make([]int, keys)
	var mu sync.Mutex
	errc := make(chan error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			for range steps {
				k := rng.IntN(keys)
				flipped, err := moveStep(p, fmt.Sprintf("k%02d", k), rng)
				if err != nil {
					errc <- err
					return
				}
				if flipped {
					mu.Lock()
					flips[k]++
					mu.Unlock()
				}
			}
			errc <- nil
		})
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		must(t, err)
	}
	if v, err := p.Invariants(ctx, 100); err != nil || len(v) != 0 {
		t.Fatalf("invariants query = %+v, %v; want empty", v, err)
	}
	c := adminConn(t, schema)
	total := 0
	for k := range keys {
		key := fmt.Sprintf("k%02d", k)
		row, err := p.Get(ctx, key)
		must(t, err)
		rows, err := c.Query(ctx, "select from_epoch from moves where database_id = $1 order by from_epoch", key)
		must(t, err)
		from, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		must(t, err)
		want := make([]int64, 0, row.Epoch)
		for e := int64(1); e < int64(row.Epoch); e++ {
			want = append(want, e)
		}
		if !slices.Equal(from, want) || len(from) != flips[k] {
			t.Fatalf("%s at epoch %d: moves from_epochs %v, %d successful flips; want %v and as many flips", key, row.Epoch, from, flips[k], want)
		}
		total += flips[k]
	}
	if total == 0 {
		t.Fatal("no flip succeeded: the race did not race")
	}
	t.Logf("%d flips over %d keys by %d workers", total, keys, workers)
}

// TestCASLatency records p50/p99 CAS latency and conflicts per second at 1, 16 and 64 workers, on distinct
// keys (no contention) and on one shared key (every worker races the same row). It runs only with
// MANSIO_PG_LATENCY=1 (about 20 s); numbers go to docs/notes/fencing/.
func TestCASLatency(t *testing.T) {
	if os.Getenv("MANSIO_PG_LATENCY") == "" {
		t.Skip("set MANSIO_PG_LATENCY=1 to measure CAS latency")
	}
	p, _ := newPlacement(t, conns64)
	const run = 3 * time.Second
	t.Logf("%-8s %7s %9s %9s %9s %10s %12s", "mode", "workers", "cas", "p50", "p99", "cas/s", "conflicts/s")
	for _, shared := range []bool{false, true} {
		for _, workers := range []int{1, 16, 64} {
			mode := "distinct"
			if shared {
				mode = "shared"
			}
			prefix := fmt.Sprintf("%s-%d-", mode, workers)
			keyOf := func(w int) string {
				if shared {
					return prefix + "k"
				}
				return fmt.Sprintf("%s%d", prefix, w)
			}
			created := workers
			if shared {
				created = 1
			}
			for w := range created {
				must(t, p.CAS(ctx, keyOf(w), owner.Place{}, owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceActive}))
			}
			lat := make([][]time.Duration, workers)
			conflicts := make([]int, workers)
			errc := make(chan error, workers)
			deadline := time.Now().Add(run)
			var wg sync.WaitGroup
			for w := range workers {
				wg.Go(func() {
					key := keyOf(w)
					for time.Now().Before(deadline) {
						row, err := p.Get(ctx, key)
						if err != nil {
							errc <- err
							return
						}
						next := row
						next.Gen++
						start := time.Now()
						err = p.CAS(ctx, key, row, next)
						lat[w] = append(lat[w], time.Since(start))
						if errors.Is(err, owner.ErrConflict) {
							conflicts[w]++
						} else if err != nil {
							errc <- err
							return
						}
					}
					errc <- nil
				})
			}
			wg.Wait()
			close(errc)
			for err := range errc {
				must(t, err)
			}
			all := slices.Concat(lat...)
			slices.Sort(all)
			nConf := 0
			for _, c := range conflicts {
				nConf += c
			}
			q := func(f float64) time.Duration { return all[min(len(all)-1, int(f*float64(len(all))))] }
			t.Logf("%-8s %7d %9d %9v %9v %10.0f %12.0f", mode, workers, len(all), q(0.50).Round(time.Microsecond),
				q(0.99).Round(time.Microsecond), float64(len(all))/run.Seconds(), float64(nConf)/run.Seconds())
		}
	}
}
