package front

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/ticket"
)

// Kind says whether a request reads or writes. It decides the ticket permission needed and whether the actor
// runs it inside the batch's write transaction.
type Kind string

const (
	KindRead  Kind = "read"
	KindWrite Kind = "write"
)

// Perm is the ticket permission k needs.
func (k Kind) Perm() ticket.Perm {
	if k == KindWrite {
		return ticket.Write
	}
	return ticket.Read
}

// Request is one decoded, validated request.
//
//	Invariant (after DecodeRequest): DB is a valid id, Kind is read or write, SQL passed checkSQL, len(Args) <= MaxArgs,
//	every arg is nil, int64, float64, string or []byte.
type Request struct {
	DB        string
	RequestID string
	Kind      Kind
	SQL       string
	Args      []any
}

// Limits bound what one request may carry. The zero value is not usable: WithDefaults fills it.
type Limits struct {
	MaxBody int // bytes of request body; default 1 MiB
	MaxSQL  int // bytes of SQL; default 64 KiB
	MaxArgs int // default 999 (SQLite's historical bind limit)
	MaxRows int // rows a read may return; more is bad_request (add LIMIT); default 10,000
}

// WithDefaults fills zero fields.
func (l Limits) WithDefaults() Limits {
	if l.MaxBody <= 0 {
		l.MaxBody = 1 << 20
	}
	if l.MaxSQL <= 0 {
		l.MaxSQL = 64 << 10
	}
	if l.MaxArgs <= 0 {
		l.MaxArgs = 999
	}
	if l.MaxRows <= 0 {
		l.MaxRows = 10_000
	}
	return l
}

// MaxRequestID bounds request_id.
const MaxRequestID = 64

func bad(format string, args ...any) error {
	return &BadRequestError{Msg: fmt.Sprintf(format, args...)}
}

// wireRequest is the JSON body. Unknown fields are rejected.
type wireRequest struct {
	DB        string            `json:"db"`
	RequestID string            `json:"request_id"`
	Kind      Kind              `json:"kind"`
	SQL       string            `json:"sql"`
	Args      []json.RawMessage `json:"args"`
	Op        json.RawMessage   `json:"op"` // reserved for compiled operations; refused for now
}

// DecodeRequest parses and validates a request body. It never panics; every failure is a *BadRequestError
// whose message describes the input.
//
//	pre:  l = l.WithDefaults()
//	post: err == nil ⇒ the Request invariant holds
func DecodeRequest(body []byte, l Limits) (Request, error) {
	if len(body) > l.MaxBody {
		return Request{}, &BadRequestError{Msg: fmt.Sprintf("body over %d bytes", l.MaxBody), Status: 413}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var w wireRequest
	if err := dec.Decode(&w); err != nil {
		return Request{}, bad("body is not a valid request object: %s", jsonProblem(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return Request{}, bad("body has data after the request object")
	}
	if w.Op != nil {
		return Request{}, bad("op payloads are not supported by this agent yet; send sql")
	}
	if err := checkID("db", w.DB, ticket.MaxDBID); err != nil {
		return Request{}, err
	}
	if w.RequestID != "" || w.Kind == KindWrite {
		if err := checkID("request_id", w.RequestID, MaxRequestID); err != nil {
			return Request{}, err
		}
	}
	if w.Kind != KindRead && w.Kind != KindWrite {
		return Request{}, bad("kind must be \"read\" or \"write\"")
	}
	if len(w.SQL) > l.MaxSQL {
		return Request{}, bad("sql over %d bytes", l.MaxSQL)
	}
	if err := checkSQL(w.Kind, w.SQL); err != nil {
		return Request{}, err
	}
	if len(w.Args) > l.MaxArgs {
		return Request{}, bad("more than %d args", l.MaxArgs)
	}
	r := Request{DB: w.DB, RequestID: w.RequestID, Kind: w.Kind, SQL: w.SQL}
	if len(w.Args) > 0 {
		r.Args = make([]any, len(w.Args))
	}
	for i, raw := range w.Args {
		v, err := decodeArg(raw)
		if err != nil {
			return Request{}, bad("args[%d]: %v", i, err)
		}
		r.Args[i] = v
	}
	return r, nil
}

// jsonProblem names a decoding failure without echoing large input back.
func jsonProblem(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return strings.TrimPrefix(s, "json: ")
}

// checkID allows 1..max bytes of [A-Za-z0-9._:-], not starting with '.' (ids become file names in some hosts).
func checkID(field, s string, max int) error {
	if s == "" || len(s) > max {
		return bad("%s must be 1..%d bytes", field, max)
	}
	if s[0] == '.' {
		return bad("%s must not start with '.'", field)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-' || c == ':') {
			return bad("%s may hold only letters, digits and . _ : -", field)
		}
	}
	return nil
}

// Leading keywords each kind may use. Transaction control (BEGIN, COMMIT, SAVEPOINT...), ATTACH, PRAGMA and
// VACUUM are never allowed: the actor owns the transaction, and the connection must not reach other files.
var (
	readKeywords  = map[string]bool{"SELECT": true, "WITH": true, "VALUES": true}
	writeKeywords = map[string]bool{"SELECT": true, "WITH": true, "VALUES": true, "INSERT": true, "UPDATE": true,
		"DELETE": true, "REPLACE": true, "CREATE": true, "DROP": true, "ALTER": true}
)

// checkSQL is the defense-in-depth guard (see the package comment): one statement, an allowed leading keyword,
// and a read must not look like a write.
func checkSQL(k Kind, sql string) error {
	if !utf8.ValidString(sql) || strings.IndexByte(sql, 0) >= 0 {
		return bad("sql must be UTF-8 without NUL bytes")
	}
	s := strings.TrimSpace(sql)
	s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	if s == "" {
		return bad("sql is empty")
	}
	if strings.Contains(s, ";") {
		return bad("one statement per request (no ';' inside; pass values as args)")
	}
	n := 0
	for n < len(s) && ('a' <= s[n]|0x20 && s[n]|0x20 <= 'z') {
		n++
	}
	kw := strings.ToUpper(s[:n])
	allowed := readKeywords
	if k == KindWrite {
		allowed = writeKeywords
	}
	if !allowed[kw] {
		return bad("%s requests may not start with %q", k, s[:min(n, 16)])
	}
	if k == KindRead && actor.IsWrite(s) {
		return bad("read request looks like a write; send kind \"write\"")
	}
	return nil
}

// decodeArg turns one JSON value into a SQLite bind value: null → nil, true/false → 1/0, an integer literal →
// int64 (must fit), any other number → float64, a string → string, {"b64": "..."} → []byte (base64, std or url).
// Arrays and other objects are errors.
func decodeArg(raw json.RawMessage) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty value")
	}
	switch raw[0] {
	case 'n':
		return nil, nil
	case 't':
		return int64(1), nil
	case 'f':
		return int64(0), nil
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("bad string")
		}
		return s, nil
	case '{':
		var b struct {
			B64 *string `json:"b64"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil || b.B64 == nil {
			return nil, fmt.Errorf("an object arg must be {\"b64\": \"<base64>\"}")
		}
		v, err := decodeB64(*b.B64)
		if err != nil {
			return nil, fmt.Errorf("b64: not base64")
		}
		return v, nil
	case '[':
		return nil, fmt.Errorf("arrays are not bind values")
	}
	s := string(raw)
	if !strings.ContainsAny(s, ".eE") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer %s does not fit in 64 bits", s[:min(len(s), 24)])
		}
		return v, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("number out of range")
	}
	return v, nil
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not base64")
}

// Blob is how a []byte value goes on the wire, in args and rows.
type Blob struct {
	B64 string `json:"b64"`
}

// wireValues replaces []byte values with Blob so rows encode losslessly.
func wireValues(rows [][]any) [][]any {
	for _, row := range rows {
		for i, v := range row {
			if b, ok := v.([]byte); ok {
				row[i] = Blob{B64: base64.StdEncoding.EncodeToString(b)}
			}
		}
	}
	return rows
}
