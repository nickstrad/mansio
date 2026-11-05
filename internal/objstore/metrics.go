package objstore

import (
	"sync/atomic"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/errs"
)

// Operation names for Metrics (Release It guide §P, object-storage guide O1). One name per kind of HTTP
// request this package sends; a durable write-back is put_gen + head + get_manifest + publish. The durable
// ones are durable's names, so request counts mean the same for every Store (durable.RequestCounter).
const (
	OpGetManifest = durable.OpGetManifest // GET manifest.json
	OpFetch       = durable.OpFetch       // GET a generation
	OpPutGen      = durable.OpPutGen      // PUT a generation (create-only)
	OpPublish     = durable.OpPublish     // PUT manifest.json (conditional)
	OpHead        = durable.OpHead        // HEAD after an upload
	OpList        = durable.OpList        // one listing page
	OpDelete      = durable.OpDelete      // one batch delete
	OpGet         = "get"                 // GetToFile
	OpPut         = "put"                 // PutFile
	OpBucket      = "bucket"              // bucket check or create
)

var opNames = []string{OpGetManifest, OpFetch, OpPutGen, OpPublish, OpHead, OpList, OpDelete, OpGet, OpPut, OpBucket}

// LatencyBuckets are the upper bounds of the latency histogram; a last, unbounded bucket follows.
var LatencyBuckets = []time.Duration{
	time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond,
	50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second,
	2500 * time.Millisecond, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute,
}

// OpStats counts the attempts (HTTP requests, retries included) of one operation.
type OpStats struct {
	ByKind  map[Kind]int64 // attempts by outcome; OK = succeeded
	Retried int64          // failed attempts that the retry layer followed with another attempt
	Bytes   int64          // body bytes moved by successful attempts
	Latency []int64        // counts per LatencyBuckets bound, plus one for longer
}

// Requests is the number of attempts.
func (o OpStats) Requests() int64 {
	var n int64
	for _, c := range o.ByKind {
		n += c
	}
	return n
}

type opCounters struct {
	byKind  [Canceled + 1]atomic.Int64
	retried atomic.Int64
	bytes   atomic.Int64
	latency []atomic.Int64
}

// metrics is built once in New and read-only afterwards except for its atomic counters.
type metrics map[string]*opCounters

func newMetrics() metrics {
	m := metrics{}
	for _, op := range opNames {
		m[op] = &opCounters{latency: make([]atomic.Int64, len(LatencyBuckets)+1)}
	}
	return m
}

// Metrics returns a snapshot of the per-operation counters since New, for every operation.
func (s *Store) Metrics() map[string]OpStats {
	out := make(map[string]OpStats, len(s.metrics))
	for op, c := range s.metrics {
		st := OpStats{ByKind: map[Kind]int64{}, Retried: c.retried.Load(), Bytes: c.bytes.Load(),
			Latency: make([]int64, len(c.latency))}
		for k := range c.byKind {
			if n := c.byKind[k].Load(); n > 0 {
				st.ByKind[Kind(k)] = n
			}
		}
		for i := range c.latency {
			st.Latency[i] = c.latency[i].Load()
		}
		out[op] = st
	}
	return out
}

// RequestCounts is attempts by operation, retries included (durable.RequestCounter).
func (s *Store) RequestCounts() map[string]int64 {
	out := make(map[string]int64, len(s.metrics))
	for op, st := range s.Metrics() {
		if n := st.Requests(); n > 0 {
			out[op] = n
		}
	}
	return out
}

// observe records one finished attempt of op on object name. A retried attempt is logged (WARN,
// S3.REQUEST_RETRIED) because nothing else will report it; a final failure is returned to the caller, who
// logs it, so it is only counted here.
//
//	pre: op is one of the Op* names
func (s *Store) observe(op, name string, kind Kind, d time.Duration, retried bool, err error) {
	c := s.metrics[op]
	c.byKind[kind].Add(1)
	i := 0
	for i < len(LatencyBuckets) && d > LatencyBuckets[i] {
		i++
	}
	c.latency[i].Add(1)
	if retried {
		c.retried.Add(1)
		s.cfg.Logger.Warn("S3.REQUEST_RETRIED", "op", op, "object", name, "kind", kind.String(),
			"duration_ms", d.Milliseconds(), "error", err.Error(), "chain", errs.Chain(err))
	}
}

// moved counts n body bytes of a successful op.
func (s *Store) moved(op string, n int64) { s.metrics[op].bytes.Add(n) }
