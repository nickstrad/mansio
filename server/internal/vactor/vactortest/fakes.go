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
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/mansio/server/internal/det"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// ErrInjected is returned by every switch-triggered failure.
var ErrInjected = errors.New("vactortest: injected failure")

// Fetcher serves objects from memory and writes them to the requested path.
type Fetcher struct {
	mu      sync.Mutex
	objects map[string][]byte
	calls   map[string]int

	Down  bool          // every Fetch fails with ErrInjected
	Delay time.Duration // each Fetch sleeps this long first (or until ctx ends), on Clock
	// Clock is the time Delay waits on and timings read (nil: real time). Set it before the first Fetch; a
	// simulation passes its manual clock so a slow fetch ends in simulated time.
	Clock det.Clock
	// Hold, if set, makes each Fetch wait for a receive from it (or until ctx ends) before answering: a fetch
	// stays in flight until the test lets it go.
	Hold chan struct{}
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
	f.mu.Lock()
	f.calls[key]++
	data, ok := f.objects[key]
	down, delay, hold, clk := f.Down, f.Delay, f.Hold, det.Or(f.Clock)
	f.mu.Unlock()
	t0 := clk.Now()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return vactor.Fetched{}, ctx.Err()
		}
	}

	if delay > 0 {
		if err := det.Sleep(ctx, clk, delay); err != nil {
			return vactor.Fetched{}, err
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
	ttfb := det.Since(clk, t0)
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return vactor.Fetched{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return vactor.Fetched{}, err
	}
	return vactor.Fetched{Bytes: int64(len(data)), TTFB: ttfb, Total: det.Since(clk, t0)}, nil
}

// Opener opens fake databases over existing files and counts opens and closes.
//
// The fake databases implement vactor.DurableDB: the file's bytes are the database's contents. A query
// "put:<value>" is a write: it replaces the file's contents with <value> and counts one commit. Snapshot
// copies the file. Any other query is a read.
type Opener struct {
	mu             sync.Mutex
	opened, closed int

	FailOpen     bool // Open fails with ErrInjected
	FailQuery    bool // every Query on databases from this opener fails with ErrInjected
	FailSnapshot bool // Snapshot fails with ErrInjected (after writing part of the file)
	HangSnapshot bool // Snapshot waits until its ctx ends and fails with the ctx's error (set before use)
}

// SetFailSnapshot sets FailSnapshot under the lock.
func (o *Opener) SetFailSnapshot(fail bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.FailSnapshot = fail
}

// Put is the fake write query that stores value.
func Put(value string) string { return "put:" + value }

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

func (o *Opener) Open(path string, _ uint64) (vactor.DB, error) {
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
	o       *Opener
	path    string
	closed  bool
	commits uint64
}

func (d *db) Query(ctx context.Context, q string) error {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if v, ok := strings.CutPrefix(q, "put:"); ok {
		if err := os.WriteFile(d.path, []byte(v), 0o644); err != nil {
			return err
		}
		d.commits++
	}
	return nil
}

func (d *db) Commits() uint64 {
	d.o.mu.Lock()
	defer d.o.mu.Unlock()
	return d.commits
}

func (d *db) Snapshot(ctx context.Context, path string) (uint64, error) {
	if d.o.HangSnapshot {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	d.o.mu.Lock()
	defer d.o.mu.Unlock()
	if d.closed {
		return d.commits, errors.New("vactortest: snapshot of closed db")
	}
	if _, err := os.Stat(path); err == nil {
		return d.commits, fmt.Errorf("vactortest: snapshot target %s exists", path)
	}
	data, err := os.ReadFile(d.path)
	if err != nil {
		return d.commits, err
	}
	if d.o.FailSnapshot {
		os.WriteFile(path, data[:len(data)/2], 0o644)
		return d.commits, fmt.Errorf("snapshot %s: %w", d.path, ErrInjected)
	}
	return d.commits, os.WriteFile(path, data, 0o644)
}

func (d *db) Close() {
	d.o.mu.Lock()
	defer d.o.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.o.closed++
	}
}

// SizedFetcher is a Fetcher that is also a vactor.Sizer: the host learns each object's size before it
// fetches, as coldload's S3 fetcher does with a HEAD.
type SizedFetcher struct{ *Fetcher }

// Size returns the stored object's length; Down and a missing object fail like Fetch.
func (f SizedFetcher) Size(ctx context.Context, key string) (int64, error) {
	f.mu.Lock()
	data, ok := f.objects[key]
	down := f.Down
	f.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return 0, ctx.Err()
	case down:
		return 0, fmt.Errorf("size %s: %w", key, ErrInjected)
	case !ok:
		return 0, fmt.Errorf("size %s: %w", key, vactor.ErrNotFound)
	}
	return int64(len(data)), nil
}
