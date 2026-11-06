package durable_test

import (
	"math"
	"testing"

	"github.com/nickstrad/mansio/server/internal/durable"
)

func TestRequestCost(t *testing.T) {
	p := durable.Prices{ClassA: 10, ClassB: 1}
	for _, tc := range []struct {
		name   string
		counts map[string]int64
		want   float64
	}{
		{"none", nil, 0},
		{"write-back: 2 PUT + HEAD + GET", map[string]int64{durable.OpPutGen: 1, durable.OpPublish: 1, durable.OpHead: 1, durable.OpGetManifest: 1}, 22},
		{"activation: 2 GET", map[string]int64{durable.OpGetManifest: 1, durable.OpFetch: 1}, 2},
		{"list is class A, delete free", map[string]int64{durable.OpList: 3, durable.OpDelete: 100}, 30},
		{"unknown ops are class B", map[string]int64{"bucket": 2}, 2},
	} {
		if got := durable.RequestCost(tc.counts, p); got != tc.want {
			t.Errorf("%s: RequestCost = %g, want %g", tc.name, got, tc.want)
		}
	}
	// One million warm write-backs at list price: 2M class A + 2M class B = $10 + $0.80.
	wb := map[string]int64{durable.OpPutGen: 1e6, durable.OpPublish: 1e6, durable.OpHead: 1e6, durable.OpGetManifest: 1e6}
	if got := durable.RequestCost(wb, durable.S3Standard); math.Abs(got-10.8) > 1e-9 {
		t.Errorf("1M write-backs cost $%g, want $10.80", got)
	}
}
