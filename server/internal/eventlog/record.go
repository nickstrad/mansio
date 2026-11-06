package eventlog

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Level is an event's level (core/events.ts Level).
type Level int8

// The zero Level is Info.
const (
	Debug Level = iota - 1
	Info
	Warn
	Error
)

var levelNames = [...]string{"debug", "info", "warn", "error"}

func (l Level) String() string {
	if l < Debug || l > Error {
		return "info"
	}
	return levelNames[l+1]
}

// ParseLevel reads a level name; ok is false for anything else.
func ParseLevel(s string) (Level, bool) {
	i := slices.Index(levelNames[:], s)
	if i < 0 {
		return Info, false
	}
	return Level(i - 1), true
}

// Event is one record before the log stamps it: the envelope fields the caller knows, and one payload. The
// log adds eventId, ts (from Time, or its clock), role and pid.
type Event struct {
	Level        Level
	Time         time.Time // zero: the log's clock at Emit
	TraceID      string    // required by the envelope; "" is written as null
	SpanID       string    // "" = absent
	ParentSpanID string
	AppID        string
	Payload      Payload
}

// Payload is one kind's fields: HTTP, Handler, SQL, Tx, Queue, Backup or Lifecycle.
type Payload interface {
	// Kind is the record's kind (the union discriminant).
	Kind() string
	encode(e *encoder)
}

// HTTP is one answered request (http_events).
type HTTP struct {
	Method, Path string
	HandlerKind  string // "query" or "mutation"
	Name         string
	Status       int
	Duration     time.Duration
	BytesIn      int64
	BytesOut     int64
	ErrorTag     string // "" = none
	ErrorDetail  any    // redacted; nil = none
}

// Handler is one handled request's execution (platform_events, event_kind handler).
type Handler struct {
	HandlerKind, Name string
	Outcome           string // "ok" or "error"
	Duration          time.Duration
	ErrorTag          string
	ErrorDetail       any
}

// SQL is one statement (sql_events). Bind values are never logged.
type SQL struct {
	Engine, Dialect string // "sqlite", "sqlite"
	StatementKind   string // select, insert, update, delete, ddl, pragma
	Path            string // SQLite step path; "" = absent (not tracked here)
	TableName       string
	SQLText         string
	BindCount       int
	Duration        time.Duration
	RowCount        int64 // negative = absent
	Outcome         string
	ErrorTag        string
	ErrorDetail     any
}

// Tx is one transaction (tx_events).
type Tx struct {
	Engine      string
	Outcome     string // commit, rollback or interrupted
	Operations  int
	Duration    time.Duration
	QueueWait   time.Duration
	ErrorTag    string
	ErrorDetail any
}

// Queue is a queue signal (queue_events): queue "events" with event "dropped" is the log's own report.
type Queue struct {
	Queue   string
	Depth   int
	Cap     int
	Event   string // high-water, rejected or dropped
	Dropped int64
}

// Backup is one backup phase or object transfer (backup_events).
type Backup struct {
	Phase       string
	Emitter     string // "backups" or "object-store"
	RunID       string
	Key         string
	Bytes       int64 // negative = absent
	SHA256      string
	Attempts    int // negative = absent
	Duration    time.Duration
	Outcome     string
	ErrorTag    string
	ErrorDetail any
}

// Lifecycle is a process or subsystem state change (lifecycle_events).
type Lifecycle struct {
	Phase  string
	Detail any
}

func (HTTP) Kind() string      { return "http" }
func (Handler) Kind() string   { return "handler" }
func (SQL) Kind() string       { return "sql" }
func (Tx) Kind() string        { return "tx" }
func (Queue) Kind() string     { return "queue" }
func (Backup) Kind() string    { return "backup" }
func (Lifecycle) Kind() string { return "lifecycle" }

// The kind keys are core/events.ts KIND_KEYS, in that order; absent optionals are written as null.

func (p HTTP) encode(e *encoder) {
	e.str("method", p.Method)
	e.str("path", p.Path)
	e.str("handlerKind", p.HandlerKind)
	e.str("name", p.Name)
	e.int("status", int64(p.Status))
	e.ms("durationMs", p.Duration)
	e.int("bytesIn", p.BytesIn)
	e.int("bytesOut", p.BytesOut)
	e.opt("errorTag", p.ErrorTag)
	e.any("errorDetail", p.ErrorDetail)
}

func (p Handler) encode(e *encoder) {
	e.str("handlerKind", p.HandlerKind)
	e.str("name", p.Name)
	e.str("outcome", p.Outcome)
	e.ms("durationMs", p.Duration)
	e.opt("errorTag", p.ErrorTag)
	e.any("errorDetail", p.ErrorDetail)
}

func (p SQL) encode(e *encoder) {
	e.str("engine", p.Engine)
	e.str("dialect", p.Dialect)
	e.str("statementKind", p.StatementKind)
	e.opt("path", p.Path)
	e.opt("tableName", p.TableName)
	e.str("sqlText", p.SQLText)
	e.int("bindCount", int64(p.BindCount))
	e.any("bindValues", nil) // never logged (core/events.ts: opt-in only; agentd has no opt-in)
	e.ms("durationMs", p.Duration)
	e.optInt("rowCount", p.RowCount)
	e.str("outcome", p.Outcome)
	e.opt("errorTag", p.ErrorTag)
	e.any("errorDetail", p.ErrorDetail)
}

func (p Tx) encode(e *encoder) {
	e.str("engine", p.Engine)
	e.str("outcome", p.Outcome)
	e.int("operations", int64(p.Operations))
	e.ms("durationMs", p.Duration)
	e.ms("queueWaitMs", p.QueueWait)
	e.opt("errorTag", p.ErrorTag)
	e.any("errorDetail", p.ErrorDetail)
}

func (p Queue) encode(e *encoder) {
	e.str("queue", p.Queue)
	e.int("depth", int64(p.Depth))
	e.int("cap", int64(p.Cap))
	e.str("event", p.Event)
	e.int("dropped", p.Dropped)
}

func (p Backup) encode(e *encoder) {
	e.str("phase", p.Phase)
	e.str("emitter", p.Emitter)
	e.opt("runId", p.RunID)
	e.opt("key", p.Key)
	e.optInt("bytes", p.Bytes)
	e.opt("sha256", p.SHA256)
	e.optInt("attempts", int64(p.Attempts))
	e.ms("durationMs", p.Duration)
	e.str("outcome", p.Outcome)
	e.opt("errorTag", p.ErrorTag)
	e.any("errorDetail", p.ErrorDetail)
}

func (p Lifecycle) encode(e *encoder) {
	e.str("phase", p.Phase)
	e.any("detail", p.Detail)
}

// stamp is what the log adds to an Event.
type stamp struct {
	EventID string
	TS      time.Time
	Role    string
	PID     int
}

// Line is ev as one canonical JSON line (core/events.ts toJsonLine), without the newline: envelope keys, kind,
// then the kind's keys, in fixed order; every string redacted (Bearer tokens, X-Amz-* URL parameters) and
// newline-free; nested object keys sorted, secret-named keys (authorization, cookie, *token, S3 keys) replaced
// by "<redacted>", nesting past 32 levels by "<max-depth>".
func (s stamp) line(ev Event) []byte {
	e := &encoder{}
	e.b.WriteByte('{')
	e.str("eventId", s.EventID)
	e.str("ts", s.TS.UTC().Format("2006-01-02T15:04:05.000Z"))
	e.str("level", ev.Level.String())
	e.str("role", s.Role)
	e.int("pid", int64(s.PID))
	e.opt("traceId", ev.TraceID)
	e.opt("spanId", ev.SpanID)
	e.opt("parentSpanId", ev.ParentSpanID)
	e.opt("appId", ev.AppID)
	e.str("kind", ev.Payload.Kind())
	ev.Payload.encode(e)
	e.b.WriteByte('}')
	return e.b.Bytes()
}

// encoder writes one JSON object's members.
type encoder struct {
	b     bytes.Buffer
	comma bool
}

func (e *encoder) key(k string) {
	if e.comma {
		e.b.WriteByte(',')
	}
	e.comma = true
	writeString(&e.b, k)
	e.b.WriteByte(':')
}

func (e *encoder) str(k, v string) {
	e.key(k)
	writeString(&e.b, clean(v))
}

func (e *encoder) opt(k, v string) {
	if v == "" {
		e.key(k)
		e.b.WriteString("null")
		return
	}
	e.str(k, v)
}

func (e *encoder) int(k string, v int64) {
	e.key(k)
	e.b.WriteString(strconv.FormatInt(v, 10))
}

func (e *encoder) optInt(k string, v int64) {
	if v < 0 {
		e.key(k)
		e.b.WriteString("null")
		return
	}
	e.int(k, v)
}

// ms writes d in milliseconds, to the microsecond.
func (e *encoder) ms(k string, d time.Duration) {
	e.key(k)
	e.b.WriteString(strconv.FormatFloat(math.Round(float64(d)/1e3)/1e3, 'f', -1, 64))
}

func (e *encoder) any(k string, v any) {
	e.key(k)
	writeValue(&e.b, v, 1)
}

// maxDepth matches core/events.ts MAX_SERIALIZE_DEPTH.
const maxDepth = 32

// writeValue writes v redacted. It walks what encoding/json decodes into (map[string]any, []any, string,
// float64, bool, nil, json.Number) and int, int64; anything else is written as its encoding/json form
// re-decoded, so its strings and keys are redacted too.
func writeValue(b *bytes.Buffer, v any, depth int) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return
	case string:
		writeString(b, clean(x))
		return
	case bool:
		b.WriteString(strconv.FormatBool(x))
		return
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			b.WriteString("null") // JSON.stringify's answer
			return
		}
		b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
		return
	case int:
		b.WriteString(strconv.Itoa(x))
		return
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
		return
	case json.Number:
		b.WriteString(x.String())
		return
	}
	if depth >= maxDepth {
		writeString(b, "<max-depth>")
		return
	}
	switch x := v.(type) {
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, item, depth+1)
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		for i, k := range slices.Sorted(maps.Keys(x)) {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, newlines.ReplaceAllString(k, " "))
			b.WriteByte(':')
			if secretKey(k) {
				writeString(b, "<redacted>")
			} else {
				writeValue(b, x[k], depth+1)
			}
		}
		b.WriteByte('}')
	default:
		raw, err := json.Marshal(x)
		var decoded any
		if err == nil {
			d := json.NewDecoder(bytes.NewReader(raw))
			d.UseNumber()
			err = d.Decode(&decoded)
		}
		if err != nil {
			writeString(b, "<unserializable>")
			return
		}
		writeValue(b, decoded, depth)
	}
}

// writeString writes s as a JSON string: JSON.stringify's escapes (no HTML escaping), with invalid UTF-8 as
// U+FFFD.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// The redaction rules are core/events.ts's, verbatim.
var (
	bearer   = regexp.MustCompile(`(?i)Bearer\s+\S+`)
	amzParam = regexp.MustCompile(`(?i)X-Amz-(Signature|Credential|Security-Token)=[^&\s"']+`)
	newlines = regexp.MustCompile("[\r\n\u2028\u2029]+")
)

var secretSuffixes = []string{"authorization", "accesskeyid", "secretaccesskey", "sessiontoken", "securitytoken", "cookie", "token"}

// secretKey reports whether a member named k is redacted whatever its value: k lower-cased, without
// separators, ends in a secret suffix (AWS_SECRET_ACCESS_KEY, Proxy-Authorization, COLUMN_APP_TOKEN, …).
func secretKey(k string) bool {
	n := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return -1
	}, k)
	for _, s := range secretSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// clean is a string value as the log may hold it: Bearer tokens and presigned-URL credentials redacted, line
// breaks replaced by a space.
func clean(s string) string {
	if strings.ContainsAny(s, "BbXx") {
		s = bearer.ReplaceAllString(s, "Bearer <redacted>")
		s = amzParam.ReplaceAllString(s, "X-Amz-${1}=<redacted>")
	}
	return newlines.ReplaceAllString(s, " ")
}
