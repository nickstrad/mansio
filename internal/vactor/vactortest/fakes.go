// Package vactortest has fakes for vactor's ports and a contract suite every Fetcher must pass.
//
// The fakes have public switches so tests and fuzzers can break them on purpose. All are mutex-guarded so
// -race stays clean.
package vactortest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/nickstrad/ts-db-go/internal/vactor"
)

// ErrInjected is returned by every switch-triggered failure.
var ErrInjected = errors.New("vactortest: injected failure")

// Fetcher serves objects from memory and writes them to the requested path.
type Fetcher struct {
	mu      sync.Mutex
	objects map[string][]byte
	calls   map[string]int

	Down  bool          // every Fetch fails with ErrInjected
	Delay time.Duration // each Fetch sleeps this long first (or until ctx ends)
}

func NewFetcher() *Fetcher {
	return &Fetcher{objects: map[string][]byte{}, calls: map[string]int{}}
}

// Put stores an object.
func (f *Fetcher) Put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
}

// SetDown sets the Down switch under the lock (for use while fetches run).
func (f *Fetcher) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Down = down
}

// Calls counts Fetch calls for key, failed ones included.
func (f *Fetcher) Calls(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

// TotalCalls counts all Fetch calls.
func (f *Fetcher) TotalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *Fetcher) Fetch(ctx context.Context, key, path string) (vactor.Fetched, error) {
	t0 := time.Now()
	f.mu.Lock()
	f.calls[key]++
	data, ok := f.objects[key]
	down, delay := f.Down, f.Delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return vactor.Fetched{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return vactor.Fetched{}, err
	}
	if down {
		return vactor.Fetched{}, fmt.Errorf("fetch %s: %w", key, ErrInjected)
	}
	if !ok {
		return vactor.Fetched{}, fmt.Errorf("fetch %s: %w", key, vactor.ErrNotFound)
	}
	ttfb := time.Since(t0)
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return vactor.Fetched{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return vactor.Fetched{}, err
	}
	return vactor.Fetched{Bytes: int64(len(data)), TTFB: ttfb, Total: time.Since(t0)}, nil
}

// Opener opens fake databases over existing files and counts opens and closes.
type Opener struct {
	mu             sync.Mutex
	opened, closed int

	FailOpen  bool // Open fails with ErrInjected
	FailQuery bool // every Query on databases from this opener fails with ErrInjected
}

// Set changes switches under the lock.
func (o *Opener) Set(failOpen, failQuery bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.FailOpen, o.FailQuery = failOpen, failQuery
}

// Counts returns how many databases were opened and closed.
func (o *Opener) Counts() (opened, closed int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened, o.closed
}

func (o *Opener) Open(path string) (vactor.DB, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.FailOpen {
		return nil, fmt.Errorf("open %s: %w", path, ErrInjected)
	}
	if _, err := os.Stat(path); err != nil { // like the real opener: never create a missing file
		return nil, err
	}
	o.opened++
	return &db{o: o, path: path}, nil
}

type db struct {
	o      *Opener
	path   string
	closed bool
}

func (d *db) Query(ctx context.Context, _ string) error {
	d.o.mu.Lock()
	defer d.o.mu.Unlock()
	switch {
	case d.closed:
		return errors.New("vactortest: query on closed db")
	case d.o.FailQuery:
		return fmt.Errorf("query %s: %w", d.path, ErrInjected)
	}
	if _, err := os.Stat(d.path); err != nil {
		return fmt.Errorf("vactortest: db file vanished under an open db: %w", err)
	}
	return ctx.Err()
}

func (d *db) Close() {
	d.o.mu.Lock()
	defer d.o.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.o.closed++
	}
}
