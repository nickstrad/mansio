package vactor

import "os"

// Crash simulates the process dying while databases are warm: every database is closed without a
// write-back and the cache directory is removed. Test-only.
func Crash(h *Host) {
	h.mu.Lock()
	h.closed = true
	entries := h.entries
	h.entries = map[string]*entry{}
	h.mu.Unlock()
	h.cancel()
	for _, e := range entries {
		<-e.done
		if e.db != nil {
			e.db.Close()
		}
	}
	os.RemoveAll(h.opts.CacheDir)
}

// FetchGate reports the fetch bulkhead's fetches and bytes in flight and its queue length. Test-only.
func FetchGate(h *Host) (n int, bytes int64, queued int) { return h.gate.inFlight() }

// WaitFor polls cond until it holds (5 s at most). Test-only.
var WaitFor = waitFor
