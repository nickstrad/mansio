package metrics

import (
	"maps"
	"slices"
	"strings"
)

// ReasonFull is the rejection reason of a queue or slot pool that had no room.
const ReasonFull = "full"

// QueueStat is one bounded queue (or slot pool that never queues) at scrape time: how much it holds at most,
// how often it refused, and how long admitted work waited in it. Queue is one of a fixed list of names per
// process (never a tenant, database or path), so the families below stay bounded.
type QueueStat struct {
	Queue    string
	Capacity int               // the most that may wait (for a pool without a queue: the most that may run)
	Rejected map[string]int64  // refusals since start by reason (ReasonFull, ...); a missing reason is not emitted
	Wait     *LatencyHistogram // time from enqueue to pickup; nil: not measured
}

// Queues adds collect, which runs once per scrape, to the registry's per-queue backpressure families:
// mansio_queue_capacity, mansio_queue_rejected_total{reason} and mansio_queue_wait_seconds, each labeled
// queue=<QueueStat.Queue>. Components call it as they are wired; the first call registers the families. Depth
// stays in each component's own gauges; capacity is what makes it a fraction.
//
//	pre: across all calls on one registry, each queue name is returned at most once per scrape
func Queues(reg *Registry, collect func() []QueueStat) {
	reg.mu.Lock()
	first := reg.queues == nil
	reg.queues = append(reg.queues, collect)
	reg.mu.Unlock()
	if !first {
		return
	}
	var qs []QueueStat
	reg.OnScrape(func() {
		reg.mu.Lock()
		sources := slices.Clone(reg.queues)
		reg.mu.Unlock()
		qs = qs[:0]
		for _, collect := range sources {
			qs = append(qs, collect()...)
		}
		slices.SortFunc(qs, func(a, b QueueStat) int { return strings.Compare(a.Queue, b.Queue) })
	})
	reg.Add("mansio_queue_capacity", "Capacity of each bounded queue or slot pool (compare with its depth or in-flight gauge).", Gauge,
		func(e *Emit) {
			for _, q := range qs {
				e.Value(Labels{"queue", q.Queue}, float64(q.Capacity))
			}
		})
	reg.Add("mansio_queue_rejected_total", "Work each queue or slot pool refused, by reason.", Counter, func(e *Emit) {
		for _, q := range qs {
			for _, reason := range slices.Sorted(maps.Keys(q.Rejected)) {
				e.Value(Labels{"queue", q.Queue, "reason", reason}, float64(q.Rejected[reason]))
			}
		}
	})
	reg.Add("mansio_queue_wait_seconds", "Time work waited in a queue before it was picked up (or gave up waiting).", Histogram,
		func(e *Emit) {
			for _, q := range qs {
				if q.Wait != nil {
					bounds, counts, sum := q.Wait.Snapshot()
					e.Histogram(Labels{"queue", q.Queue}, bounds, counts, sum)
				}
			}
		})
}
