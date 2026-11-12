package vactor

// Gauges are the host's levels at one instant, for the agent's /metrics (per pool or queue,
// in use and queued). Counters since NewHost are in Stats; oldest_dirty_age is in each TickReport.
type Gauges struct {
	Active             int   // keys active or activating
	FetchesInFlight    int   // fetch bulkhead: fetches running
	FetchBytesInFlight int64 // ...and their bytes
	FetchQueued        int   // activations waiting for a fetch slot (MaxFetchQueue bounds it)
	FetchQueueCap      int   // Options.MaxFetchQueue after defaults
}

// Gauges reads the levels (read-only; O(1)).
func (h *Host) Gauges() Gauges {
	n, b, q := h.gate.inFlight()
	return Gauges{Active: h.Active(), FetchesInFlight: n, FetchBytesInFlight: b, FetchQueued: q, FetchQueueCap: h.opts.MaxFetchQueue}
}
