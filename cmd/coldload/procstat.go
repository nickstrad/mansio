package main

import (
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"

	"github.com/nickstrad/ts-db-go/internal/coldload"
)

// snapshot reads Go runtime memory, current RSS (via ps: includes SQLite's non-Go allocations) and open file
// descriptors. It runs between levels, never inside a measured window.
func snapshot() coldload.Snapshot {
	runtime.GC()
	ms := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}}
	metrics.Read(ms)
	return coldload.Snapshot{
		Goroutines: runtime.NumGoroutine(),
		FDs:        countFDs(),
		HeapMB:     float64(ms[0].Value.Uint64()) / (1 << 20),
		TotalMB:    float64(ms[1].Value.Uint64()) / (1 << 20),
		RSSMB:      rssMB(),
	}
}

// rssMB is the current resident set size, or -1 if ps fails.
func rssMB() float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	kb, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return -1
	}
	return kb / 1024
}

// countFDs counts open file descriptors via /dev/fd (macOS and Linux), or -1. Same as internal/stats.
func countFDs() int {
	d, err := os.Open("/dev/fd")
	if err != nil {
		return -1
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return -1
	}
	return len(names) - 1 // minus the descriptor used to read the directory
}
