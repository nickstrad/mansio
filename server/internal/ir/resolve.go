package ir

// Resolve returns value with every {"$ref": i} replaced by ids[i], the _id insert i produced (core/ir/
// semantics.ts resolve). Unlike decode, it walks every object, so a $ref nested in a plain object is
// replaced too; validation rejects that object anyway. Decode has checked each i, so the lookup hits.
//
//	pre: every $ref in value is an index into ids (DecodeValue guarantees it for a decoded request)
func Resolve(value any, ids []string) any {
	switch x := value.(type) {
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = Resolve(item, ids)
		}
		return out
	case *Object:
		if r, ok := x.Get("$ref"); ok {
			return ids[int(r.(float64))]
		}
		out := NewObject()
		for _, k := range x.keys {
			out.Set(k, Resolve(x.vals[k], ids))
		}
		return out
	}
	return value
}

// ResolveID is the literal id of an id position.
func ResolveID(id IDRef, ids []string) string {
	if id.IsRef {
		return ids[id.Ref]
	}
	return id.ID
}
