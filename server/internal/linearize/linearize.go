// Package linearize checks a concurrent history against a sequential model: is there an order of the
// operations, consistent with real time, in which every operation's output is what the model answers? It is
// the Wing–Gong algorithm with Lowe's memoization (as Porcupine), small enough to read: the IR-5 Jepsen-style
// run (cmd/controld, plan §6.5) checks each database's client history with it.
//
// An operation whose outcome the client never learned (no answer to a mutation) has Return = Unknown: it may
// have taken effect at any time after its call, or never, and its output is ignored. Pure and deterministic;
// test code and tools only.
package linearize

import (
	"fmt"
	"math"
	"slices"
)

// Unknown is the Return of an operation that may still be in flight forever (its outcome was never learned).
const Unknown int64 = math.MaxInt64

// Op is one operation of a history: the client's call and return times (any monotonic unit), its input and,
// when it returned, its output.
type Op[I, O any] struct {
	Call, Return int64
	Input        I
	Output       O
}

// Model is a sequential specification over states S. Step answers whether the operation with input in and
// output out is legal in state s, and the state after it. For an operation with Return == Unknown (unknown
// is true), out means nothing: Step gives its effect if it took effect then. The checker may also leave such
// an operation out (it never happened).
type Model[S, I, O any] struct {
	Init func() S
	// Step: pre: s is not modified (copy before changing).
	Step func(s S, in I, out O, unknown bool) (ok bool, next S)
	// Key is a state's identity for memoization: equal states must give equal keys.
	Key func(s S) string
}

// Result is a check's verdict.
type Result struct {
	OK bool
	// Longest is the longest linearizable prefix found (operation indexes in linearization order), for
	// a failing history's report.
	Longest []int
	// Explored counts search steps (memo hits included): a measure of the history's difficulty.
	Explored int
	// GaveUp is true when the search passed MaxSteps without a verdict (OK is then false).
	GaveUp bool
}

// MaxSteps bounds one check (a pathological history must not hang a test run).
var MaxSteps = 50_000_000

type entry struct {
	op        int
	call      bool
	time      int64
	prev, nxt *entry
	match     *entry // a call's return entry
}

// Check reports whether ops is linearizable under m.
func Check[S, I, O any](m Model[S, I, O], ops []Op[I, O]) Result {
	// The event list: calls and returns by time; at equal times calls first (overlap is the safe reading).
	type ev struct {
		t    int64
		call bool
		op   int
	}
	evs := make([]ev, 0, 2*len(ops))
	for i, o := range ops {
		if o.Return < o.Call {
			panic(fmt.Sprintf("linearize: op %d returns (%d) before its call (%d)", i, o.Return, o.Call))
		}
		evs = append(evs, ev{o.Call, true, i}, ev{o.Return, false, i})
	}
	slices.SortStableFunc(evs, func(a, b ev) int {
		switch {
		case a.t != b.t:
			if a.t < b.t {
				return -1
			}
			return 1
		case a.call != b.call:
			if a.call {
				return -1
			}
			return 1
		}
		return 0
	})
	head := &entry{}
	cur := head
	calls := make([]*entry, len(ops))
	for _, e := range evs {
		n := &entry{op: e.op, call: e.call, time: e.t, prev: cur}
		cur.nxt = n
		cur = n
		if e.call {
			calls[e.op] = n
		} else {
			calls[e.op].match = n
		}
	}

	type frame struct {
		e     *entry
		state S
	}
	state := m.Init()
	linearized := make([]bool, len(ops))
	seen := map[string]struct{}{}
	var stack []frame
	var order, longest []int
	res := Result{}
	lift := func(e *entry) { // unlink a call and its return
		e.prev.nxt = e.nxt
		if e.nxt != nil {
			e.nxt.prev = e.prev
		}
		r := e.match
		r.prev.nxt = r.nxt
		if r.nxt != nil {
			r.nxt.prev = r.prev
		}
	}
	unlift := func(e *entry) {
		r := e.match
		r.prev.nxt = r
		if r.nxt != nil {
			r.nxt.prev = r
		}
		e.prev.nxt = e
		if e.nxt != nil {
			e.nxt.prev = e
		}
	}
	memo := func(s S) string {
		b := make([]byte, (len(ops)+7)/8)
		for i, l := range linearized {
			if l {
				b[i/8] |= 1 << (i % 8)
			}
		}
		return string(b) + "|" + m.Key(s)
	}
	e := head.nxt
	for e != nil {
		res.Explored++
		if res.Explored > MaxSteps {
			res.GaveUp, res.Longest = true, longest
			return res
		}
		// Only the unknown operations remain: each may be left out, so the history is linearizable.
		if e.call && allUnknown(e, ops) {
			res.OK = true
			return res
		}
		if e.call {
			o := ops[e.op]
			unknown := o.Return == Unknown
			ok, next := m.Step(state, o.Input, o.Output, unknown)
			if ok {
				linearized[e.op] = true
				key := memo(next)
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					stack = append(stack, frame{e, state})
					state = next
					order = append(order, e.op)
					if len(order) > len(longest) {
						longest = slices.Clone(order)
					}
					lift(e)
					e = head.nxt
					continue
				}
				linearized[e.op] = false
			}
			e = e.nxt
			continue
		}
		// A return whose call is not linearized yet: backtrack.
		if len(stack) == 0 {
			res.Longest = longest
			return res
		}
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		order = order[:len(order)-1]
		state = top.state
		linearized[top.e.op] = false
		unlift(top.e)
		e = top.e.nxt
	}
	res.OK = true
	return res
}

// allUnknown reports whether every entry from e on belongs to an unknown operation (so all may be dropped).
func allUnknown[I, O any](e *entry, ops []Op[I, O]) bool {
	for ; e != nil; e = e.nxt {
		if ops[e.op].Return != Unknown {
			return false
		}
	}
	return true
}

// Partition splits ops by key (independent sub-histories, e.g. one per register): a history is linearizable
// iff each part is, and checking parts is exponentially cheaper.
func Partition[I, O any](ops []Op[I, O], key func(I) string) map[string][]Op[I, O] {
	out := map[string][]Op[I, O]{}
	for _, o := range ops {
		k := key(o.Input)
		out[k] = append(out[k], o)
	}
	return out
}
