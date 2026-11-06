package ir_test

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/ir"
)

// indexBody is a body near the 64 KiB cap whose insert value has only array-index member names, in the
// given order: the worst case for ECMAScript-ordered objects.
func indexBody(order string) []byte {
	head := `{"v":1,"db":{"tenant":"t","app":"a"},"kind":"mutation","requestId":"r","schema":"sha256:` +
		strings.Repeat("0", 64) + `","ops":[{"op":"insert","table":"t","value":{`
	var names []int
	for n, size := 0, len(head); size < ir.MaxBodyBytes-16; n++ {
		names = append(names, n)
		size += len(strconv.Itoa(n)) + 5
	}
	switch order {
	case "descending":
		for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
			names[i], names[j] = names[j], names[i]
		}
	case "shuffled":
		rand.New(rand.NewPCG(1, 2)).Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	}
	members := make([]string, len(names))
	for i, n := range names {
		members[i] = `"` + strconv.Itoa(n) + `":0`
	}
	return []byte(head + strings.Join(members, ",") + "}}]}")
}

var indexOrders = []string{"ascending", "descending", "shuffled"}

// TestIndexNamesDecodeFast bounds the decode of a cap-sized body of array-index names (it was quadratic:
// ~615 ms against 37 ms in TS). The bound is generous for a loaded CI machine; it is ~1.5 ms on a laptop.
func TestIndexNamesDecodeFast(t *testing.T) {
	for _, order := range indexOrders {
		body := indexBody(order)
		start := time.Now()
		req, err := ir.DecodeBody(body)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s: %v", order, err)
		}
		keys := req.Ops[0].(*ir.Insert).Value.Keys()
		if keys[0] != "0" || keys[1] != "1" || keys[len(keys)-1] != strconv.Itoa(len(keys)-1) {
			t.Errorf("%s: keys not in ascending numeric order", order)
		}
		if elapsed > 50*time.Millisecond {
			t.Errorf("%s: %d bytes, %d names decoded in %v", order, len(body), len(keys), elapsed)
		}
	}
}

func BenchmarkDecodeIndexNames(b *testing.B) {
	for _, order := range indexOrders {
		body := indexBody(order)
		b.Run(order, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, err := ir.DecodeBody(body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSetIndexNames is the ordered insert used outside the parser.
func BenchmarkSetIndexNames(b *testing.B) {
	for b.Loop() {
		o := ir.NewObject()
		for n := range 7000 {
			o.Set(strconv.Itoa(n), 0.0)
		}
	}
}
