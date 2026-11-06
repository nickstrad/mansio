package coldload_test

import (
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/coldload"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

func col(row []string, name string) string {
	for i, h := range coldload.ActivationHeader {
		if h == name {
			return row[i]
		}
	}
	panic("no column " + name)
}

func TestRecordRow(t *testing.T) {
	lv := coldload.Level{Index: 3, Kind: coldload.KindDup, Conc: 50, Keys: []string{"k0", "k1"},
		Class: coldload.SizeClass{Name: "1g", Bytes: 1 << 30}}
	owner := coldload.Record{Level: lv, Seq: 1, Start: time.Millisecond, Wait: 2 * time.Second,
		Res: vactor.Result{Act: &vactor.Activation{Bytes: 42, Fetch: 1500 * time.Microsecond, Total: 2 * time.Second}}}
	waiter := coldload.Record{Level: lv, Seq: 0, Wait: time.Second, Res: vactor.Result{Waited: true},
		Err: errors.New("bad, \"quoted\"\nsecond line")}

	tests := []struct {
		name string
		rec  coldload.Record
		want map[string]string
	}{
		{"owner carries stage timings", owner, map[string]string{"level": "3", "key": "k1", "bytes": "42",
			"fetch_ms": "1.500", "total_ms": "2000.000", "get_ms": "2000.000", "waited": "false", "err": "ok",
			"target_bytes": "1073741824", "concurrency": "50"}},
		{"waiter has only its own wait", waiter, map[string]string{"key": "k0", "bytes": "0", "fetch_ms": "0.000",
			"total_ms": "0.000", "get_ms": "1000.000", "waited": "true", "err": "bad, \"quoted\" second line"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.rec.Row()
			if len(row) != len(coldload.ActivationHeader) {
				t.Fatalf("row has %d fields, header %d", len(row), len(coldload.ActivationHeader))
			}
			for k, v := range tc.want {
				if got := col(row, k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestLevelSummaryRow(t *testing.T) {
	s := coldload.LevelSummary{
		Level:  coldload.Level{Kind: coldload.KindCold, Conc: 4, Keys: make([]string, 10)},
		Result: coldload.LevelResult{OK: 10, Bytes: 20e6, Wall: 2 * time.Second},
	}
	row := s.Row()
	if len(row) != len(coldload.LevelHeader) {
		t.Fatalf("row has %d fields, header %d", len(row), len(coldload.LevelHeader))
	}
	if row[9] != "5.00" || row[10] != "10.00" { // act_per_s, mb_per_s
		t.Fatalf("rates = %s, %s; want 5.00 act/s and 10.00 MB/s", row[9], row[10])
	}
	s.Result.Wall = 0 // boundary: no division by zero
	if row := s.Row(); row[9] != "0" {
		t.Fatalf("act_per_s with zero wall = %s", row[9])
	}
}
