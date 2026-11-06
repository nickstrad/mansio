package ir

// Limits (spec §3, §4; core/ir/types.ts).
const (
	MaxOps       = 64     // operations per request
	MaxBodyBytes = 65_536 // request body cap, bytes
	MaxIDBytes   = 64     // longest requestId, db.tenant or db.app
)

// Kind is a request kind.
type Kind string

const (
	Query    Kind = "query"
	Mutation Kind = "mutation"
)

// Request is a decoded /v1/ops body.
type Request struct {
	DB   DB
	Kind Kind
	// RequestID is "" when absent (a wire id is never empty). Required for a mutation.
	RequestID string
	// Schema is the fingerprint the client compiled against: sha256: + 64 lowercase hex.
	Schema string
	Ops    []Op
}

// DB names the database.
type DB struct{ Tenant, App string }

// Op is one operation: *Select, *Get, *Insert, *Patch, *Delete or *Assert.
type Op interface {
	// Name is the wire op name.
	Name() string
	// TableName is the op's table member (not yet checked against a schema).
	TableName() string
	json() *Object
}

// IDRef is an id position: a literal id, or a reference to the id produced by insert number Ref.
type IDRef struct {
	ID    string
	Ref   int
	IsRef bool
}

// Filter is one select filter. Op is "eq" or "gt" after decode; Value is any JSON value and may hold
// {"$ref": i} objects (resolve them with Resolve). Code outside decode may build any Op, and Undefined for
// an absent value; the compiler is total over both.
type Filter struct {
	Field string
	Op    string
	Value any
}

// Order is a select ordering. Direction is "asc" or "desc" after decode.
type Order struct{ Field, Direction string }

// Aggregate is {"kind":"count"} or {"kind":"countBy","field":…}.
type Aggregate struct{ Kind, Field string }

// Expect is a precondition (spec §5.4). Nil members are absent.
type Expect struct {
	Version *int64
	Exists  *bool
	Fields  *Object
}

// Select is the select op: core/plan.ts's SelectPlan without kind. Limit is any number; its range is an
// execution check.
type Select struct {
	Table     string
	Filters   []Filter
	Order     *Order
	Limit     float64
	Aggregate *Aggregate
}

// Get reads one document by id.
type Get struct {
	Table string
	ID    IDRef
}

// Insert writes a new document. ID is the client-generated UUIDv7, or "" for a server id.
type Insert struct {
	Table string
	Value *Object
	ID    string
}

// Patch merges Value into a document.
type Patch struct {
	Table  string
	ID     IDRef
	Value  *Object
	Expect *Expect
}

// Delete removes a document.
type Delete struct {
	Table  string
	ID     IDRef
	Expect *Expect
}

// Assert checks a precondition and writes nothing.
type Assert struct {
	Table  string
	ID     IDRef
	Expect *Expect
}

func (*Select) Name() string { return "select" }
func (*Get) Name() string    { return "get" }
func (*Insert) Name() string { return "insert" }
func (*Patch) Name() string  { return "patch" }
func (*Delete) Name() string { return "delete" }
func (*Assert) Name() string { return "assert" }

func (o *Select) TableName() string { return o.Table }
func (o *Get) TableName() string    { return o.Table }
func (o *Insert) TableName() string { return o.Table }
func (o *Patch) TableName() string  { return o.Table }
func (o *Delete) TableName() string { return o.Table }
func (o *Assert) TableName() string { return o.Table }

// JSON is the request as a JSON value, the shape core/ir/decode.ts produces; Canonical(r.JSON()) is the
// canonical encoding of spec §9.
func (r *Request) JSON() *Object {
	db := NewObject("tenant", r.DB.Tenant, "app", r.DB.App)
	out := NewObject("v", 1.0, "db", db, "kind", string(r.Kind), "schema", r.Schema)
	ops := make([]any, len(r.Ops))
	for i, op := range r.Ops {
		ops[i] = op.json()
	}
	out.Set("ops", ops)
	if r.RequestID != "" {
		out.Set("requestId", r.RequestID)
	}
	return out
}

func (id IDRef) json() any {
	if id.IsRef {
		return NewObject("$ref", float64(id.Ref))
	}
	return id.ID
}

func (e *Expect) json() any {
	if e == nil {
		return Undefined
	}
	out := NewObject()
	if e.Version != nil {
		out.Set("version", float64(*e.Version))
	}
	if e.Fields != nil {
		out.Set("fields", e.Fields)
	}
	if e.Exists != nil {
		out.Set("exists", *e.Exists)
	}
	return out
}

func (o *Select) json() *Object {
	filters := make([]any, len(o.Filters))
	for i, f := range o.Filters {
		filters[i] = NewObject("field", f.Field, "op", f.Op, "value", f.Value)
	}
	out := NewObject("op", "select", "table", o.Table, "filters", filters, "limit", o.Limit)
	if o.Order != nil {
		out.Set("order", NewObject("field", o.Order.Field, "direction", o.Order.Direction))
	}
	if o.Aggregate != nil {
		agg := NewObject("kind", o.Aggregate.Kind)
		if o.Aggregate.Kind == "countBy" {
			agg.Set("field", o.Aggregate.Field)
		}
		out.Set("aggregate", agg)
	}
	return out
}

func (o *Get) json() *Object { return NewObject("op", "get", "table", o.Table, "id", o.ID.json()) }

func (o *Insert) json() *Object {
	out := NewObject("op", "insert", "table", o.Table, "value", o.Value)
	if o.ID != "" {
		out.Set("id", o.ID)
	}
	return out
}

func (o *Patch) json() *Object {
	return NewObject("op", "patch", "table", o.Table, "id", o.ID.json(), "value", o.Value, "expect", o.Expect.json())
}

func (o *Delete) json() *Object {
	return NewObject("op", "delete", "table", o.Table, "id", o.ID.json(), "expect", o.Expect.json())
}

func (o *Assert) json() *Object {
	return NewObject("op", "assert", "table", o.Table, "id", o.ID.json(), "expect", o.Expect.json())
}
