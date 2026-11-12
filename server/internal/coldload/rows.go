package coldload

import (
	"strconv"
	"strings"
	"time"
)

// ActivationHeader is the header of activations.csv; Record.Row fills the same columns in the same order.
var ActivationHeader = []string{"level", "size_class", "target_bytes", "concurrency", "seq", "key", "bytes",
	"local_hit", "start_ms", "get_ms", "total_ms", "ttfb_ms", "fetch_ms", "open_ms", "first_query_ms", "warm_ms",
	"waited", "err"}

// Row is r as one activations.csv row. Stage timings come only from the caller that started the activation;
// a caller that waited or found the key active has zeros there (its own time is get_ms).
func (r Record) Row() []string {
	var a struct {
		bytes                                 int64
		localHit                              bool
		total, ttfb, fetch, open, first, warm time.Duration
	}
	if act := r.Res.Act; act != nil {
		a.bytes, a.localHit = act.Bytes, act.LocalHit
		a.total, a.ttfb, a.fetch, a.open, a.first, a.warm = act.Total, act.TTFB, act.Fetch, act.Open, act.FirstQuery, act.Warm
	}
	errText := "ok"
	if r.Err != nil {
		errText = strings.ReplaceAll(r.Err.Error(), "\n", " ")
	}
	l := r.Level
	return []string{strconv.Itoa(l.Index), l.Class.Name, strconv.FormatInt(l.Class.Bytes, 10), strconv.Itoa(l.Conc),
		strconv.Itoa(r.Seq), l.Keys[r.Seq], strconv.FormatInt(a.bytes, 10), strconv.FormatBool(a.localHit),
		Millis(r.Start), Millis(r.Wait), Millis(a.total), Millis(a.ttfb), Millis(a.fetch), Millis(a.open),
		Millis(a.first), Millis(a.warm), strconv.FormatBool(r.Res.Waited), errText}
}

// Snapshot is the process's resource use at one moment (filled in by cmd/bench/coldload).
type Snapshot struct {
	Goroutines, FDs        int
	HeapMB, TotalMB, RSSMB float64
}

// LevelSummary is one levels.csv row.
type LevelSummary struct {
	Level         Level
	Result        LevelResult
	Fetches       int64 // fetches the host made during the level
	Before, After Snapshot
	Passivate     time.Duration
	Dropped       int64 // activation records the recorder dropped during the level
}

// LevelHeader is the header of levels.csv; LevelSummary.Row fills the same columns in the same order.
var LevelHeader = []string{"level", "kind", "size_class", "target_bytes", "concurrency", "n", "ok", "errors",
	"wall_ms", "act_per_s", "mb_per_s", "fetches", "goroutines_before", "goroutines_after", "go_heap_mb_before",
	"go_heap_mb_after", "go_total_mb_before", "go_total_mb_after", "rss_mb_before", "rss_mb_after", "fds_before",
	"fds_after", "passivate_ms", "records_dropped"}

func (s LevelSummary) Row() []string {
	l, r := s.Level, s.Result
	sec := r.Wall.Seconds()
	rate := func(v float64) string {
		if sec <= 0 {
			return "0"
		}
		return f2(v / sec)
	}
	return []string{strconv.Itoa(l.Index), l.Kind, l.Class.Name, strconv.FormatInt(l.Class.Bytes, 10),
		strconv.Itoa(l.Conc), strconv.Itoa(len(l.Keys)), strconv.Itoa(r.OK), strconv.Itoa(r.Failed),
		Millis(r.Wall), rate(float64(r.OK)), rate(float64(r.Bytes) / 1e6), strconv.FormatInt(s.Fetches, 10),
		strconv.Itoa(s.Before.Goroutines), strconv.Itoa(s.After.Goroutines), f2(s.Before.HeapMB), f2(s.After.HeapMB),
		f2(s.Before.TotalMB), f2(s.After.TotalMB), f2(s.Before.RSSMB), f2(s.After.RSSMB),
		strconv.Itoa(s.Before.FDs), strconv.Itoa(s.After.FDs), Millis(s.Passivate), strconv.FormatInt(s.Dropped, 10)}
}

// Millis formats d as milliseconds with microsecond precision, e.g. "1.250".
func Millis(d time.Duration) string { return strconv.FormatFloat(float64(d)/1e6, 'f', 3, 64) }

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
