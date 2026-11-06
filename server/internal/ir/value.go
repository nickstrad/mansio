package ir

import (
	"math"
	"slices"
	"sort"
	"strconv"
)

// Undefined is the JavaScript undefined: an absent value, written Undefined{} (a type, so no package can
// reassign it; compare with v == (Undefined{})). An object member set to Undefined is absent, an array
// element that is Undefined encodes as null, and an error field that is Undefined is dropped.
type Undefined struct{}

// Object is a JSON object with ECMAScript own-property order: names that are array indices ("0" to
// "4294967294", canonical decimal) come first in ascending numeric order, then the other names in the order
// they were first set. JSON.stringify, Object.keys and Object.entries all use this order.
//
// The zero value is not usable; use NewObject.
type Object struct {
	keys []string
	vals map[string]any
	// idx holds the numeric values of the leading array-index names, keys[:len(idx)], ascending, so an
	// insert is a binary search (and an append for ascending names) rather than a rescan.
	idx []uint64
}

// NewObject returns an empty object; kv are name/value pairs set in order.
func NewObject(kv ...any) *Object {
	o := &Object{vals: make(map[string]any, len(kv)/2)}
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// objectOf builds an object from distinct names in insertion order in O(n log n), whatever the order of
// the array-index names (the parser's path: untrusted input must not make Set's ordered insert quadratic).
func objectOf(names []string, vals map[string]any) *Object {
	o := &Object{vals: vals}
	type indexed struct {
		n    uint64
		name string
	}
	var index []indexed
	var rest []string
	for _, name := range names {
		if n, ok := arrayIndex(name); ok {
			index = append(index, indexed{n, name})
		} else {
			rest = append(rest, name)
		}
	}
	sort.Slice(index, func(i, j int) bool { return index[i].n < index[j].n })
	o.keys = make([]string, 0, len(names))
	o.idx = make([]uint64, len(index))
	for i, x := range index {
		o.keys = append(o.keys, x.name)
		o.idx[i] = x.n
	}
	o.keys = append(o.keys, rest...)
	return o
}

// Len is the number of members.
func (o *Object) Len() int { return len(o.keys) }

// Keys returns the member names in ECMAScript order. The caller must not modify the slice.
func (o *Object) Keys() []string { return o.keys }

// Get returns the member's value, or (Undefined, false) when it is absent.
func (o *Object) Get(name string) (any, bool) {
	v, ok := o.vals[name]
	if !ok {
		return Undefined{}, false
	}
	return v, true
}

// Value returns the member's value, or Undefined.
func (o *Object) Value(name string) any {
	v, _ := o.Get(name)
	return v
}

// Has reports whether the member is present (JavaScript Object.hasOwn).
func (o *Object) Has(name string) bool {
	_, ok := o.vals[name]
	return ok
}

// Set sets a member. A new name takes its ECMAScript position; an existing one keeps its position.
// Setting Undefined removes the member.
func (o *Object) Set(name string, v any) {
	if v == (Undefined{}) {
		o.remove(name)
		return
	}
	if _, ok := o.vals[name]; ok {
		o.vals[name] = v
		return
	}
	o.vals[name] = v
	n, isIndex := arrayIndex(name)
	if !isIndex {
		o.keys = append(o.keys, name)
		return
	}
	at := len(o.idx)
	if at > 0 && o.idx[at-1] > n {
		at = sort.Search(len(o.idx), func(i int) bool { return o.idx[i] > n })
	}
	o.idx = slices.Insert(o.idx, at, n)
	o.keys = slices.Insert(o.keys, at, name)
}

func (o *Object) remove(name string) {
	if _, ok := o.vals[name]; !ok {
		return
	}
	delete(o.vals, name)
	i := slices.Index(o.keys, name)
	o.keys = slices.Delete(o.keys, i, i+1)
	if i < len(o.idx) {
		o.idx = slices.Delete(o.idx, i, i+1)
	}
}

// arrayIndex reports whether name is an ECMAScript array index, and its value.
func arrayIndex(name string) (uint64, bool) {
	if name == "" || len(name) > 10 || (name[0] == '0' && len(name) > 1) {
		return 0, false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(name, 10, 64)
	if err != nil || n > math.MaxUint32-1 {
		return 0, false
	}
	return n, true
}

// MaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, 2^53 - 1.
const MaxSafeInteger = 1<<53 - 1

// IsInteger is JavaScript's Number.isInteger.
func IsInteger(v any) bool {
	f, ok := v.(float64)
	return ok && !math.IsInf(f, 0) && f == math.Trunc(f)
}

// IsSafeInteger is JavaScript's Number.isSafeInteger.
func IsSafeInteger(v any) bool {
	f, ok := v.(float64)
	return ok && IsInteger(f) && math.Abs(f) <= MaxSafeInteger
}

// Equal is deep equality of JSON values; numbers compare as doubles (so 0 equals -0), objects regardless
// of member order, arrays in order.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case *Object:
		y, ok := b.(*Object)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for _, k := range x.keys {
			w, ok := y.Get(k)
			if !ok || !Equal(x.vals[k], w) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
