package linearize

import (
	"strconv"
	"testing"
)

// A register: write v, read → v, cas(old, new) → ok.
type regIn struct {
	kind   string // "r", "w", "cas"
	v, old int
}

type regOut struct {
	v  int
	ok bool
}

var register = Model[int, regIn, regOut]{
	Init: func() int { return 0 },
	Step: func(s int, in regIn, out regOut, unknown bool) (bool, int) {
		switch in.kind {
		case "w":
			return true, in.v
		case "r":
			return unknown || out.v == s, s
		}
		if s == in.old {
			return unknown || out.ok, in.v
		}
		return unknown || !out.ok, s
	},
	Key: strconv.Itoa,
}

func op(call, ret int64, kind string, v int, out regOut) Op[regIn, regOut] {
	return Op[regIn, regOut]{Call: call, Return: ret, Input: regIn{kind: kind, v: v}, Output: out}
}

func TestRegisterHistories(t *testing.T) {
	for name, tc := range map[string]struct {
		ops  []Op[regIn, regOut]
		want bool
	}{
		"empty":      {nil, true},
		"sequential": {[]Op[regIn, regOut]{op(0, 1, "w", 1, regOut{}), op(2, 3, "r", 0, regOut{v: 1})}, true},
		"stale read": {[]Op[regIn, regOut]{op(0, 1, "w", 1, regOut{}), op(2, 3, "r", 0, regOut{v: 0})}, false},
		// Overlapping writes: either order; a later read sees one of them and must keep seeing it.
		"concurrent writes": {[]Op[regIn, regOut]{op(0, 5, "w", 1, regOut{}), op(1, 6, "w", 2, regOut{}),
			op(7, 8, "r", 0, regOut{v: 1}), op(9, 10, "r", 0, regOut{v: 1})}, true},
		"flip-flop": {[]Op[regIn, regOut]{op(0, 5, "w", 1, regOut{}), op(1, 6, "w", 2, regOut{}),
			op(7, 8, "r", 0, regOut{v: 1}), op(9, 10, "r", 0, regOut{v: 2})}, false},
		// A write with no answer may land late, or never.
		"unknown write seen late": {[]Op[regIn, regOut]{op(0, Unknown, "w", 3, regOut{}), op(1, 2, "r", 0, regOut{v: 0}),
			op(5, 6, "r", 0, regOut{v: 3})}, true},
		"unknown write never": {[]Op[regIn, regOut]{op(0, Unknown, "w", 3, regOut{}), op(1, 2, "r", 0, regOut{v: 0})}, true},
		"unknown write cannot un-happen": {[]Op[regIn, regOut]{op(0, Unknown, "w", 3, regOut{}), op(1, 2, "r", 0, regOut{v: 3}),
			op(5, 6, "r", 0, regOut{v: 0})}, false},
		"value from nowhere": {[]Op[regIn, regOut]{op(0, 1, "r", 0, regOut{v: 9})}, false},
	} {
		if got := Check(register, tc.ops); got.OK != tc.want || got.GaveUp {
			t.Errorf("%s: %+v, want ok=%v", name, got, tc.want)
		}
	}
}

// A large, honestly sequential history (each op inside a global lock) with some concurrency at the edges
// checks fast thanks to the memo.
func TestManyConcurrentOps(t *testing.T) {
	var ops []Op[regIn, regOut]
	val := 0
	for i := range 2000 {
		c := int64(i * 10)
		if i%3 == 0 {
			val = i
			ops = append(ops, op(c, c+15, "w", i, regOut{})) // overlaps the next op
		} else {
			ops = append(ops, op(c+16-10, c+9+10, "r", 0, regOut{v: val}))
		}
	}
	if got := Check(register, ops); !got.OK {
		t.Fatalf("%+v", got.GaveUp)
	}
}

func TestPartition(t *testing.T) {
	type in struct{ k string }
	parts := Partition([]Op[in, int]{{Input: in{"a"}}, {Input: in{"b"}}, {Input: in{"a"}}}, func(i in) string { return i.k })
	if len(parts["a"]) != 2 || len(parts["b"]) != 1 {
		t.Fatalf("%v", parts)
	}
}
