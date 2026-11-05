package owner_test

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/owner"
)

func benchTable(b *testing.B, keys int) (*owner.Table, []string) {
	w := newWorld(b, "a")
	names := make([]string, keys)
	for i := range names {
		names[i] = fmt.Sprintf("db-%06d", i)
		w.place(names[i], "a")
	}
	return w.tab("a"), names
}

// Admit + Done on one key, one goroutine: the uncontended hot path.
func BenchmarkAdmit(b *testing.B) {
	tab, keys := benchTable(b, 1)
	b.ReportAllocs()
	for b.Loop() {
		p, err := tab.Admit(keys[0], 1)
		if err != nil {
			b.Fatal(err)
		}
		p.Done()
	}
}

// A rejected admission (stale epoch): must be as cheap, with no allocation.
func BenchmarkAdmitRejected(b *testing.B) {
	tab, keys := benchTable(b, 1)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := tab.Admit(keys[0], 7); err == nil {
			b.Fatal("admitted")
		}
	}
}

// Every goroutine on the same key: worst case for the per-key counter's cache line.
func BenchmarkAdmitParallelOneKey(b *testing.B) {
	tab, keys := benchTable(b, 1)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p, err := tab.Admit(keys[0], 1)
			if err != nil {
				b.Fatal(err)
			}
			p.Done()
		}
	})
}

// Goroutines spread over 4096 keys: no lock or cache line shared between keys.
func BenchmarkAdmitParallelManyKeys(b *testing.B) {
	tab, keys := benchTable(b, 4096)
	var next atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := next.Add(7919)
		for pb.Next() {
			i++
			p, err := tab.Admit(keys[i%uint64(len(keys))], 1)
			if err != nil {
				b.Fatal(err)
			}
			p.Done()
		}
	})
}
