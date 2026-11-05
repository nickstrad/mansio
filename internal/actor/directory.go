package actor

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/panicx"
)

// Directory maps database i to its actor. Get is a lock-free load of an immutable snapshot, so lookups never
// contend; Grow publishes a new snapshot.
//
//	Invariant: snapshot[i].ID == i for every i.
type Directory struct {
	cfg    Config
	actors atomic.Pointer[[]*Actor]
	mu     sync.Mutex // serializes Grow and Close
}

// NewDirectory creates an empty directory; every actor it starts uses cfg.
func NewDirectory(cfg Config) *Directory {
	d := &Directory{cfg: cfg.WithDefaults()}
	d.actors.Store(&[]*Actor{})
	return d
}

func (d *Directory) snapshot() []*Actor { return *d.actors.Load() }

// Len is the number of actors.
func (d *Directory) Len() int { return len(d.snapshot()) }

// Get returns database i's actor.
//
//	pre: 0 <= i < Len()
func (d *Directory) Get(i int) *Actor { return d.snapshot()[i] }

// GrowStats says what growing the directory cost.
type GrowStats struct {
	Added int
	Seed  time.Duration // creating and filling new databases
	Open  time.Duration // opening databases and starting actors
}

// Grow adds actors until there are n. For each new database i it calls seed(i) (create and fill the file),
// then opens path(i), using up to workers goroutines. It is all or nothing: if any database fails to seed or
// open, every actor this call started is closed and the directory is unchanged.
func (d *Directory) Grow(n int, path func(i int) string, seed func(i int) error, workers int) (GrowStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.snapshot()
	start := len(old)
	if n <= start {
		return GrowStats{}, nil
	}
	t0 := time.Now()
	if err := parallel(start, n, workers, seed); err != nil {
		return GrowStats{}, err
	}
	seedDur := time.Since(t0)

	added := make([]*Actor, n-start)
	t1 := time.Now()
	err := parallel(start, n, workers, func(i int) error {
		a := New(i, path(i), d.cfg)
		if err := a.Start(true); err != nil {
			return err
		}
		added[i-start] = a
		return nil
	})
	if err != nil {
		for _, a := range added {
			if a != nil {
				a.Close()
			}
		}
		return GrowStats{}, err
	}
	next := slices.Concat(old, added)
	if assert.Enabled {
		for i, a := range next {
			assert.That(a.ID == i, "Directory: slot %d holds actor %d", i, a.ID)
		}
	}
	d.actors.Store(&next)
	return GrowStats{Added: len(added), Seed: seedDur, Open: time.Since(t1)}, nil
}

// Close stops every actor.
func (d *Directory) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	var wg sync.WaitGroup
	for _, a := range d.snapshot() {
		wg.Go(a.Close)
	}
	wg.Wait()
}

// Stats is a point-in-time view across actors.
type Stats struct {
	Open       int // databases with an open connection
	Queued     int // requests waiting in mailboxes and read queues
	MaxMailbox int // the deepest single queue
}

// Stats scans every actor once.
func (d *Directory) Stats() Stats {
	var s Stats
	for _, a := range d.snapshot() {
		if a.IsOpen() {
			s.Open++
		}
		q := a.QueueLen()
		s.Queued += q
		s.MaxMailbox = max(s.MaxMailbox, q)
	}
	return s
}

// parallel calls fn(i) for i in [from, to) on up to workers goroutines and joins every error. A panic in fn is
// recovered in its worker and re-raised here as a *panicx.Panic, after every worker has stopped.
func parallel(from, to, workers int, fn func(int) error) error {
	ch := make(chan int)
	var mu sync.Mutex
	var errList []error
	var panicked *panicx.Panic
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Go(func() {
			defer func() {
				if p := panicx.From(recover()); p != nil {
					mu.Lock()
					if panicked == nil {
						panicked = p
					}
					mu.Unlock()
					for range ch { // keep draining so the sender never blocks
					}
				}
			}()
			for i := range ch {
				if err := fn(i); err != nil {
					mu.Lock()
					errList = append(errList, err)
					mu.Unlock()
				}
			}
		})
	}
	for i := from; i < to; i++ {
		ch <- i
	}
	close(ch)
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}
	return errors.Join(errList...)
}
