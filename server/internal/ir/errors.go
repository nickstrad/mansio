package ir

import "strings"

// Wire tags produced by the protocol itself (spec §8).
const (
	TagSchema          = "SchemaError"
	TagCompile         = "CompileError"
	TagNotFound        = "NotFoundError"
	TagConflict        = "ConflictError"
	TagSchemaDrift     = "SchemaDriftError"
	TagTooLarge        = "RequestTooLarge"
	TagDecode          = "DecodeError"
	TagStorage         = "StorageError"
	TagDefect          = "Defect"
	TagStmtDeadline    = "StatementDeadlineError"
	TagAuth            = "AuthError"     // a credential (app token, API key, session) missing or wrong: 401
	TagQuota           = "QuotaExceeded" // a new database past a creation cap (agentd create_dbs): 429
	ReasonTypeMismatch = "type-mismatch"
	ReasonNotAnObject  = "not-an-object"
	ReasonUnknownField = "unknown-field"
)

// Error is a protocol failure (spec §8): a wire tag, the structured fields of the TypeScript error class
// (in that class's construction order, so a JSON rendering matches TS byte for byte), and the index of the
// operation it belongs to.
//
// It deliberately does not implement the error interface: functions return a *Error that is nil on
// success, and a nil *Error stored in an error variable would compare non-nil. Keep it as *Error until
// the edge turns it into a response (Wire); if it must travel as an error, wrap a non-nil one explicitly.
type Error struct {
	Tag    string
	Fields *Object
	// Op is the operation index; meaningful only when HasOp.
	Op    int
	HasOp bool
}

// NewError builds an error; kv are field name/value pairs in order. Undefined values are dropped.
func NewError(tag string, kv ...any) *Error {
	return &Error{Tag: tag, Fields: NewObject(kv...)}
}

// SchemaErr is core/errors.ts's SchemaError{reason, path, expected, actual}. Pass Undefined for no actual.
func SchemaErr(reason, path, expected string, actual any) *Error {
	return NewError(TagSchema, "reason", reason, "path", path, "expected", expected, "actual", actual)
}

// TooLarge is RequestTooLarge{limit, observed}.
func TooLarge(limit, observed int) *Error {
	return NewError(TagTooLarge, "limit", float64(limit), "observed", float64(observed))
}

// WithOp returns e tagged with operation index i.
func (e *Error) WithOp(i int) *Error {
	e.Op, e.HasOp = i, true
	return e
}

// Field returns a field's value, or Undefined.
func (e *Error) Field(name string) any { return e.Fields.Value(name) }

// Reason is the reason field, or "".
func (e *Error) Reason() string {
	s, _ := e.Field("reason").(string)
	return s
}

// Path is the path field, or "".
func (e *Error) Path() string {
	s, _ := e.Field("path").(string)
	return s
}

// Status is the HTTP status of the tag (core/errors.ts toHttpStatus, core/ir/errors.ts statusOf).
func (e *Error) Status() int {
	switch e.Tag {
	case TagSchema, TagCompile:
		return 400
	case TagNotFound:
		return 404
	case TagConflict, TagSchemaDrift:
		return 409
	case TagAuth:
		return 401
	case TagQuota:
		return 429
	case TagTooLarge:
		return 413
	case TagStmtDeadline:
		return 504
	}
	return 500
}

// Message is the wire message (core/ir/errors.ts toWireError): "Tag: k=v ..." for 4xx, the bare tag for
// 5xx.
func (e *Error) Message() string {
	if e.Status() >= 500 {
		return e.Tag
	}
	parts := make([]string, 0, e.Fields.Len())
	for _, k := range e.Fields.Keys() {
		parts = append(parts, k+"="+Stringify(e.Fields.vals[k]))
	}
	return e.Tag + ": " + strings.Join(parts, " ")
}

// String is Message, for fmt.
func (e *Error) String() string { return e.Message() }

// Wire is the response body {"error": {tag, message, op?, fields?}} (spec §8): fields only on 4xx.
func (e *Error) Wire() *Object {
	body := NewObject("tag", e.Tag, "message", e.Message())
	if e.HasOp {
		body.Set("op", float64(e.Op))
	}
	if e.Status() < 500 {
		body.Set("fields", e.Fields)
	}
	return NewObject("error", body)
}
