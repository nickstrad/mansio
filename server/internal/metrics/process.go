package metrics

import (
	"os"
	"runtime"
	"runtime/metrics"
)

// Process exposes the process's own steady-state gauges (for soak tests): goroutines, open file descriptors and Go
// heap bytes. Every daemon registers it so a soak can check that each one is flat.
func Process(reg *Registry) {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}}
	reg.Add("mansio_process", "Process levels: goroutines, open_fds (-1 if unknown), heap_bytes (live objects), go_bytes (all memory the Go runtime mapped).",
		Gauge, func(e *Emit) {
			metrics.Read(sample)
			e.Value(Labels{"stat", "goroutines"}, float64(runtime.NumGoroutine()))
			e.Value(Labels{"stat", "open_fds"}, float64(openFDs()))
			e.Value(Labels{"stat", "heap_bytes"}, float64(sample[0].Value.Uint64()))
			e.Value(Labels{"stat", "go_bytes"}, float64(sample[1].Value.Uint64()))
		})
}

// openFDs counts this process's open descriptors (/proc/self/fd on Linux, /dev/fd on macOS), minus the one
// the count itself opens; -1 when neither is readable.
func openFDs() int {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		d, err := os.Open(dir)
		if err != nil {
			continue
		}
		names, err := d.Readdirnames(-1) // names only: ReadDir would lstat each, and fail on /dev/fd's own
		d.Close()
		if err == nil {
			return len(names) - 1
		}
	}
	return -1
}
