package actor

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

// Benchmarks: go test ./internal/actor -run '^$' -bench . -benchtime 2s

func BenchmarkDirectPointRead(b *testing.B) { // SQLite in the same goroutine: the floor
	e, err := openSQLite(seededKV(b, Config{}, 1000), Config{}.WithDefaults(), false)
	if err != nil {
		b.Fatal(err)
	}
	defer e.close()
	c := context.Background()
	for i := 0; b.Loop(); i++ {
		if r := e.exec(c, &Request{SQL: `SELECT v FROM kv WHERE id = ?`, Args: []any{1 + i%1000}}); r.Err != nil {
			b.Fatal(r.Err)
		}
	}
}

func BenchmarkPointRead(b *testing.B) {
	a := startedKV(b, Config{}, 1000)
	c := context.Background()
	for i := 0; b.Loop(); i++ {
		if r := a.Do(c, `SELECT v FROM kv WHERE id = ?`, false, 1+i%1000); r.Err != nil {
			b.Fatal(r.Err)
		}
	}
}

func BenchmarkOneActorManyClientsMixed(b *testing.B) {
	a := startedKV(b, Config{MailboxSize: 1024}, 1000)
	c := context.Background()
	var n atomic.Int64
	b.SetParallelism(4)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := n.Add(1)
			write := i%5 == 0 // 20% writes
			q := `SELECT v FROM kv WHERE id = ?`
			args := []any{1 + i%1000}
			if write {
				q, args = `UPDATE kv SET v = ? WHERE id = ?`, []any{i, 1 + i%1000}
			}
			if r := a.Do(c, q, write, args...); r.Err != nil {
				b.Fatal(r.Err)
			}
		}
	})
}

func BenchmarkWrites(b *testing.B) {
	for _, sync := range []string{"NORMAL", "FULL"} {
		for _, batch := range []int{1, 64} {
			b.Run(fmt.Sprintf("synchronous=%s/max_batch=%d", sync, batch), func(b *testing.B) {
				a := startedKV(b, Config{MaxBatch: batch, MailboxSize: 1024, Synchronous: sync}, 0)
				c := context.Background()
				var next atomic.Int64
				b.SetParallelism(16)
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						id := next.Add(1)
						if r := a.Do(c, `INSERT INTO kv (id, v) VALUES (?, ?)`, true, id, id); r.Err != nil {
							b.Fatal(r.Err)
						}
					}
				})
			})
		}
	}
}

func BenchmarkFanOut1000Actors(b *testing.B) {
	const n = 1000
	dir := b.TempDir()
	path := func(i int) string { return filepath.Join(dir, fmt.Sprintf("db-%d.sqlite", i)) }
	d := NewDirectory(Config{})
	b.Cleanup(d.Close)
	if _, err := d.Grow(n, path, func(i int) error { return Seed(path(i), Config{}, kvDDL, fillKV(100)) }, runtime.NumCPU()); err != nil {
		b.Fatal(err)
	}
	c := context.Background()
	var k atomic.Int64
	b.SetParallelism(64)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := k.Add(1)
			if r := d.Get(int(i%n)).Do(c, `SELECT v FROM kv WHERE id = ?`, false, 1+i%100); r.Err != nil {
				b.Fatal(r.Err)
			}
		}
	})
}
