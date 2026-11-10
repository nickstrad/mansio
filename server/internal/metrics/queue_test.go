package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/metrics"
)

// Queues takes sources from several components; one scrape shows every queue once, sorted, with its
// capacity, its depth and wait histogram where measured, and its refusals by reason.
func TestQueuesExposition(t *testing.T) {
	r := metrics.NewRegistry()
	wait := metrics.NewLatency([]time.Duration{time.Millisecond})
	wait.Observe(0)
	wait.Observe(time.Second)
	metrics.Queues(r, func() []metrics.QueueStat {
		return []metrics.QueueStat{{Queue: "mailbox", Capacity: 256, Depth: metrics.NoDepth, Rejected: map[string]int64{metrics.ReasonFull: 3}, Wait: wait}}
	})
	metrics.Queues(r, func() []metrics.QueueStat { // a second component, registered later
		return []metrics.QueueStat{{Queue: "admission", Capacity: 8, Depth: 5, Rejected: map[string]int64{"would_expire": 2, metrics.ReasonFull: 1}}}
	})
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP mansio_queue_capacity Capacity of each bounded queue or slot pool (compare with its depth or in-flight gauge).
# TYPE mansio_queue_capacity gauge
mansio_queue_capacity{queue="admission"} 8
mansio_queue_capacity{queue="mailbox"} 256
# HELP mansio_queue_depth Work waiting in each bounded queue now (a slot pool: slots in use).
# TYPE mansio_queue_depth gauge
mansio_queue_depth{queue="admission"} 5
# HELP mansio_queue_rejected_total Work each queue or slot pool refused, by reason.
# TYPE mansio_queue_rejected_total counter
mansio_queue_rejected_total{queue="admission",reason="full"} 1
mansio_queue_rejected_total{queue="admission",reason="would_expire"} 2
mansio_queue_rejected_total{queue="mailbox",reason="full"} 3
# HELP mansio_queue_wait_seconds Time work waited in a queue before it was picked up (or gave up waiting).
# TYPE mansio_queue_wait_seconds histogram
mansio_queue_wait_seconds_bucket{queue="mailbox",le="0.001"} 1
mansio_queue_wait_seconds_bucket{queue="mailbox",le="+Inf"} 2
mansio_queue_wait_seconds_sum{queue="mailbox"} 1
mansio_queue_wait_seconds_count{queue="mailbox"} 2
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}
