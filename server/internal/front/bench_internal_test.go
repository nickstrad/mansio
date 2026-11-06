package front

import (
	"context"
	"encoding/json"
	"testing"
)

func BenchmarkDecodeRequest(b *testing.B) {
	body := []byte(`{"db":"db-1","request_id":"r1","kind":"read","sql":"SELECT * FROM orders WHERE id = ?","args":[42]}`)
	l := Limits{}.WithDefaults()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := DecodeRequest(body, l); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeResponse(b *testing.B) {
	r := Response{Code: OK, RequestID: "r1", Columns: []string{"id", "a", "b"}, Rows: [][]any{{int64(42), "status-7", 3.5}},
		Count: 1, Timing: &Timing{1, 7}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAdmissionAcquireRelease(b *testing.B) {
	a := NewAdmission(AdmissionLimits{TenantInFlight: 1 << 20, DBInFlight: 1 << 20})
	ctx := context.Background()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rel, err := a.Acquire(ctx, "t", "db-1")
			if err != nil {
				b.Fatal(err)
			}
			rel()
		}
	})
}
