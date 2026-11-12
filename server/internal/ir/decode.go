package ir

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	// wireID is mansio-server's front.checkID: [A-Za-z0-9._:-], 1..64 bytes, not starting with ".".
	wireID = regexp.MustCompile(`^[A-Za-z0-9_:-][A-Za-z0-9._:-]{0,63}$`)
	// UUIDv7 is a canonical lowercase UUIDv7: version nibble 7, RFC 9562 variant.
	UUIDv7      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	fingerprint = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// tenantRE and appRE are the control plane's registry domains (0002_registry.sql tenant_id and
	// database_name), narrower than wire ids: the database is stored under the placement key tenant ":" app
	// (spec §4, ADR 0011).
	tenantRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	appRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,55}$`)
	opIndexRE = regexp.MustCompile(`^ops\[(\d+)\]`)
)

var opNames = []string{"select", "get", "insert", "patch", "delete", "assert"}

// shapes are the members each operation allows, and the ones it requires (spec §5).
var shapes = map[string]struct{ allowed, required []string }{
	"select": {[]string{"op", "table", "filters", "order", "limit", "aggregate"}, []string{"table", "filters", "limit"}},
	"get":    {[]string{"op", "table", "id"}, []string{"table", "id"}},
	"insert": {[]string{"op", "table", "value", "id"}, []string{"table", "value"}},
	"patch":  {[]string{"op", "table", "id", "value", "expect"}, []string{"table", "id", "value"}},
	"delete": {[]string{"op", "table", "id", "expect"}, []string{"table", "id"}},
	"assert": {[]string{"op", "table", "id", "expect"}, []string{"table", "expect"}},
}

func mismatch(path, expected string, actual any) *Error {
	return SchemaErr(ReasonTypeMismatch, path, expected, actual)
}

// object is value as an object with only allowed members (nil: any) and every required one. An unknown
// member is reported before a missing one, each in member order.
func object(value any, path string, allowed, required []string) (*Object, *Error) {
	o, ok := value.(*Object)
	if !ok {
		at := path
		if at == "" {
			at = "body"
		}
		return nil, SchemaErr(ReasonNotAnObject, at, "object", value)
	}
	if allowed != nil {
		for _, k := range o.keys {
			if !slices.Contains(allowed, k) {
				return nil, SchemaErr(ReasonUnknownField, Member(path, k), "declared field", Undefined{})
			}
		}
	}
	for _, k := range required {
		if !o.Has(k) {
			return nil, mismatch(Member(path, k), "required member", Undefined{})
		}
	}
	return o, nil
}

func str(value any, path string) (string, *Error) {
	if s, ok := value.(string); ok {
		return s, nil
	}
	return "", mismatch(path, "string", value)
}

func literal(value any, path string, allowed []string) (string, *Error) {
	if s, ok := value.(string); ok && slices.Contains(allowed, s) {
		return s, nil
	}
	quoted := make([]string, len(allowed))
	for i, a := range allowed {
		quoted[i] = Stringify(a)
	}
	return "", mismatch(path, strings.Join(quoted, " | "), value)
}

func wireIDOf(value any, path string) (string, *Error) {
	if s, ok := value.(string); ok && wireID.MatchString(s) {
		return s, nil
	}
	return "", mismatch(path, "1.."+strconv.Itoa(MaxIDBytes)+` of [A-Za-z0-9._:-], not starting with "."`, value)
}

func registryName(value any, path string, re *regexp.Regexp) (string, *Error) {
	if s, ok := value.(string); ok && re.MatchString(s) {
		return s, nil
	}
	return "", mismatch(path, re.String(), value)
}

// decoder carries the operations decoded so far, which a $ref may target.
type decoder struct {
	ops []Op
}

// refs walks a value tree (an insert or patch field, a filter value, an expect.fields value) and checks
// every $ref in it. An object with a $ref member is a reference; any other object is left for schema
// validation to reject (and is not walked), since no document value is an object.
func (d *decoder) refs(value any, path string) *Error {
	switch x := value.(type) {
	case []any:
		for i, item := range x {
			if err := d.refs(item, Element(path, i)); err != nil {
				return err
			}
		}
	case *Object:
		if x.Has("$ref") {
			_, err := d.ref(x, path)
			return err
		}
	}
	return nil
}

// ref is {"$ref": i} with i the index of an earlier insert.
func (d *decoder) ref(value any, path string) (int, *Error) {
	r, err := object(value, path, []string{"$ref"}, []string{"$ref"})
	if err != nil {
		return 0, err
	}
	i := r.Value("$ref")
	f, ok := i.(float64)
	if !ok || !IsInteger(f) || f < 0 || f >= float64(len(d.ops)) || d.ops[int(f)].Name() != "insert" {
		return 0, mismatch(Member(path, "$ref"), "index of an earlier insert", i)
	}
	return int(f), nil
}

// idRef is an id position: a non-empty string without U+0000, or a $ref.
func (d *decoder) idRef(value any, path string) (IDRef, *Error) {
	if o, ok := value.(*Object); ok && o.Has("$ref") {
		i, err := d.ref(o, path)
		return IDRef{Ref: i, IsRef: true}, err
	}
	s, ok := value.(string)
	if !ok || s == "" || strings.Contains(s, "\x00") {
		return IDRef{}, mismatch(path, "non-empty id without U+0000, or $ref", value)
	}
	return IDRef{ID: s}, nil
}

func (d *decoder) fieldValues(value any, path string) (*Object, *Error) {
	o, err := object(value, path, nil, nil)
	if err != nil {
		return nil, err
	}
	for _, k := range o.keys {
		if err := d.refs(o.vals[k], Member(path, k)); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (d *decoder) expect(value any, path, op string) (*Expect, *Error) {
	e, err := object(value, path, []string{"version", "exists", "fields"}, nil)
	if err != nil {
		return nil, err
	}
	out := &Expect{}
	if e.Len() == 0 {
		return nil, mismatch(path, "at least one of version, exists, fields", Undefined{})
	}
	if v, ok := e.Get("version"); ok {
		if !IsSafeInteger(v) || v.(float64) < 1 {
			return nil, mismatch(Member(path, "version"), "safe integer >= 1", v)
		}
		n := int64(v.(float64))
		out.Version = &n
	}
	if v, ok := e.Get("fields"); ok {
		at := Member(path, "fields")
		fields, err := d.fieldValues(v, at)
		if err != nil {
			return nil, err
		}
		if fields.Len() == 0 {
			return nil, mismatch(at, "at least one field", Undefined{})
		}
		out.Fields = fields
	}
	if v, ok := e.Get("exists"); ok {
		at := Member(path, "exists")
		exists, isBool := v.(bool)
		if !isBool {
			return nil, mismatch(at, "boolean", v)
		}
		if !exists && (out.Version != nil || out.Fields != nil) {
			return nil, mismatch(at, "true when version or fields is present", v)
		}
		if !exists && op == "patch" {
			return nil, mismatch(at, "true on a patch", v)
		}
		out.Exists = &exists
	}
	return out, nil
}

func (d *decoder) selectOf(o *Object, path string) (Op, *Error) {
	table, err := str(o.Value("table"), Member(path, "table"))
	if err != nil {
		return nil, err
	}
	list, ok := o.Value("filters").([]any)
	if !ok {
		return nil, mismatch(Member(path, "filters"), "array", o.Value("filters"))
	}
	sel := &Select{Table: table, Filters: make([]Filter, 0, len(list))}
	for i, item := range list {
		at := Element(Member(path, "filters"), i)
		f, err := object(item, at, []string{"field", "op", "value"}, []string{"field", "op", "value"})
		if err != nil {
			return nil, err
		}
		field, err := str(f.Value("field"), Member(at, "field"))
		if err != nil {
			return nil, err
		}
		op, err := literal(f.Value("op"), Member(at, "op"), []string{"eq", "gt"})
		if err != nil {
			return nil, err
		}
		if err := d.refs(f.Value("value"), Member(at, "value")); err != nil {
			return nil, err
		}
		sel.Filters = append(sel.Filters, Filter{Field: field, Op: op, Value: f.Value("value")})
	}
	limit, ok := o.Value("limit").(float64)
	if !ok {
		return nil, mismatch(Member(path, "limit"), "number", o.Value("limit"))
	}
	sel.Limit = limit
	if v, ok := o.Get("order"); ok {
		at := Member(path, "order")
		order, err := object(v, at, []string{"field", "direction"}, []string{"field", "direction"})
		if err != nil {
			return nil, err
		}
		field, err := str(order.Value("field"), Member(at, "field"))
		if err != nil {
			return nil, err
		}
		dir, err := literal(order.Value("direction"), Member(at, "direction"), []string{"asc", "desc"})
		if err != nil {
			return nil, err
		}
		sel.Order = &Order{Field: field, Direction: dir}
	}
	if v, ok := o.Get("aggregate"); ok {
		at := Member(path, "aggregate")
		agg, err := object(v, at, []string{"kind", "field"}, []string{"kind"})
		if err != nil {
			return nil, err
		}
		kind, err := literal(agg.Value("kind"), Member(at, "kind"), []string{"count", "countBy"})
		if err != nil {
			return nil, err
		}
		if kind == "count" {
			if agg.Has("field") {
				return nil, SchemaErr(ReasonUnknownField, Member(at, "field"), "declared field", Undefined{})
			}
			sel.Aggregate = &Aggregate{Kind: kind}
		} else {
			if !agg.Has("field") {
				return nil, mismatch(Member(at, "field"), "required member", Undefined{})
			}
			field, err := str(agg.Value("field"), Member(at, "field"))
			if err != nil {
				return nil, err
			}
			sel.Aggregate = &Aggregate{Kind: kind, Field: field}
		}
	}
	return sel, nil
}

func (d *decoder) op(value any, kind Kind) (Op, *Error) {
	path := Element("ops", len(d.ops))
	head, err := object(value, path, nil, []string{"op"})
	if err != nil {
		return nil, err
	}
	name, err := literal(head.Value("op"), Member(path, "op"), opNames)
	if err != nil {
		return nil, err
	}
	if kind == Query && name != "select" && name != "get" && name != "assert" {
		return nil, mismatch(Member(path, "op"), "select | get | assert in a query", name)
	}
	shape := shapes[name]
	o, err := object(head, path, shape.allowed, shape.required)
	if err != nil {
		return nil, err
	}
	if name == "select" {
		return d.selectOf(o, path)
	}
	table, err := str(o.Value("table"), Member(path, "table"))
	if err != nil {
		return nil, err
	}
	switch name {
	case "get":
		id, err := d.idRef(o.Value("id"), Member(path, "id"))
		if err != nil {
			return nil, err
		}
		return &Get{Table: table, ID: id}, nil
	case "insert":
		v, err := d.fieldValues(o.Value("value"), Member(path, "value"))
		if err != nil {
			return nil, err
		}
		ins := &Insert{Table: table, Value: v}
		if id, ok := o.Get("id"); ok {
			s, isStr := id.(string)
			if !isStr || !UUIDv7.MatchString(s) {
				return nil, mismatch(Member(path, "id"), "lowercase canonical UUIDv7", id)
			}
			ins.ID = s
		}
		return ins, nil
	}
	if _, ok := o.Get("id"); !ok && name == "assert" {
		return d.assertTable(o, table, path)
	}
	id, err := d.idRef(o.Value("id"), Member(path, "id"))
	if err != nil {
		return nil, err
	}
	var v *Object
	if name == "patch" {
		if v, err = d.fieldValues(o.Value("value"), Member(path, "value")); err != nil {
			return nil, err
		}
	}
	var expect *Expect
	if e, ok := o.Get("expect"); ok {
		if expect, err = d.expect(e, Member(path, "expect"), name); err != nil {
			return nil, err
		}
	}
	switch name {
	case "patch":
		return &Patch{Table: table, ID: id, Value: v, Expect: expect}, nil
	case "delete":
		return &Delete{Table: table, ID: id, Expect: expect}, nil
	}
	return &Assert{Table: table, ID: id, Expect: expect}, nil
}

// assertTable decodes an assert without an id: its expect is exactly {"unchangedSince": n} (spec §5.5).
func (d *decoder) assertTable(o *Object, table, path string) (Op, *Error) {
	at := Member(path, "expect")
	e, err := object(o.Value("expect"), at, []string{"unchangedSince"}, []string{"unchangedSince"})
	if err != nil {
		return nil, err
	}
	since := e.Value("unchangedSince")
	if !IsSafeInteger(since) || since.(float64) < 0 {
		return nil, mismatch(Member(at, "unchangedSince"), "safe integer >= 0", since)
	}
	return &AssertTable{Table: table, Since: int64(since.(float64))}, nil
}

// withOp sets the error's op from its path when the path is inside ops[i].
func withOp(err *Error) *Error {
	if m := opIndexRE.FindStringSubmatch(err.Path()); m != nil {
		if i, convErr := strconv.Atoi(m[1]); convErr == nil {
			return err.WithOp(i)
		}
	}
	return err
}

// DecodeValue decodes an already-parsed JSON value (spec §4–§5, core/intermediate-representation/decode.ts decodeValue). Every
// failure is a SchemaError; one inside ops[i] carries op i.
func DecodeValue(value any) (*Request, *Error) {
	r, err := decodeValue(value)
	if err != nil {
		return nil, withOp(err)
	}
	return r, nil
}

func decodeValue(value any) (*Request, *Error) {
	top, err := object(value, "", []string{"v", "db", "kind", "requestId", "schema", "ops"},
		[]string{"v", "db", "kind", "schema", "ops"})
	if err != nil {
		return nil, err
	}
	if v := top.Value("v"); v != 1.0 {
		return nil, mismatch("v", "1", v)
	}
	db, err := object(top.Value("db"), "db", []string{"tenant", "app"}, []string{"tenant", "app"})
	if err != nil {
		return nil, err
	}
	r := &Request{}
	if r.DB.Tenant, err = registryName(db.Value("tenant"), "db.tenant", tenantRE); err != nil {
		return nil, err
	}
	if r.DB.App, err = registryName(db.Value("app"), "db.app", appRE); err != nil {
		return nil, err
	}
	kind, err := literal(top.Value("kind"), "kind", []string{"query", "mutation"})
	if err != nil {
		return nil, err
	}
	r.Kind = Kind(kind)
	if v, ok := top.Get("requestId"); ok {
		if r.RequestID, err = wireIDOf(v, "requestId"); err != nil {
			return nil, err
		}
	} else if r.Kind == Mutation {
		return nil, mismatch("requestId", "required member", Undefined{})
	}
	schema, ok := top.Value("schema").(string)
	if !ok || !fingerprint.MatchString(schema) {
		return nil, mismatch("schema", "sha256:<64 lowercase hex>", top.Value("schema"))
	}
	r.Schema = schema
	list, ok := top.Value("ops").([]any)
	if !ok || len(list) < 1 || len(list) > MaxOps {
		var actual any = top.Value("ops")
		if ok {
			actual = float64(len(list))
		}
		return nil, mismatch("ops", "array of 1.."+strconv.Itoa(MaxOps)+" operations", actual)
	}
	d := &decoder{ops: make([]Op, 0, len(list))}
	for _, item := range list {
		op, err := d.op(item, r.Kind)
		if err != nil {
			return nil, err
		}
		d.ops = append(d.ops, op)
	}
	r.Ops = d.ops
	return r, nil
}

// DecodeBody decodes a request body (spec §3): the byte cap, then UTF-8, then strict JSON, then
// DecodeValue. Over MaxBodyBytes is RequestTooLarge (413); anything else is a SchemaError.
func DecodeBody(body []byte) (*Request, *Error) {
	if len(body) > MaxBodyBytes {
		return nil, TooLarge(MaxBodyBytes, len(body))
	}
	if !utf8.Valid(body) {
		return nil, mismatch("body", "UTF-8", Undefined{})
	}
	v, err := Parse(string(body))
	if err != nil {
		return nil, withOp(err)
	}
	return DecodeValue(v)
}
