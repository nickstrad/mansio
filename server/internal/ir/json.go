package ir

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxDepth is the deepest nesting Parse accepts: a container opened inside MaxDepth others is refused.
const MaxDepth = 64

// stop is one parse failure: where (path) and why (expected).
type stop struct{ path, expected string }

// Member is path extended by an object member, in core/schema.ts's path style (a.b; a bare name at top).
func Member(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// Element is path extended by an array index (a[3]).
func Element(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// Parse reads text as one strict JSON value (spec §3 step 3, core/ir/json.ts parseStrict). Beyond the RFC
// 8259 grammar it refuses a duplicate member name (after unescaping), a number that is not a finite double,
// a string holding a lone surrogate, and nesting deeper than MaxDepth. A refusal is a SchemaError
// type-mismatch whose path names where the parser stopped (body for the top level).
//
// text must be valid UTF-8 (DecodeBody checks it first). A byte-order mark is not whitespace, so it is
// refused like any other stray character.
func Parse(text string) (any, *Error) {
	p := parser{text: text}
	v, s := p.value("", 0)
	if s == nil {
		p.skip()
		if p.at < len(p.text) {
			s = &stop{"", "JSON"}
		}
	}
	if s != nil {
		path := s.path
		if path == "" {
			path = "body"
		}
		return nil, SchemaErr(ReasonTypeMismatch, path, s.expected, Undefined)
	}
	return v, nil
}

type parser struct {
	text string
	at   int
}

func (p *parser) skip() {
	for p.at < len(p.text) {
		switch p.text[p.at] {
		case ' ', '\t', '\n', '\r':
			p.at++
		default:
			return
		}
	}
}

func (p *parser) peek() byte {
	if p.at < len(p.text) {
		return p.text[p.at]
	}
	return 0
}

func (p *parser) value(path string, depth int) (any, *stop) {
	p.skip()
	c := p.peek()
	if (c == '{' || c == '[') && depth >= MaxDepth {
		return nil, &stop{path, "nesting at most " + strconv.Itoa(MaxDepth) + " arrays/objects deep"}
	}
	switch c {
	case '{':
		return p.object(path, depth)
	case '[':
		return p.array(path, depth)
	case '"':
		return p.string(path, false)
	}
	for _, lit := range [...]struct {
		word string
		v    any
	}{{"true", true}, {"false", false}, {"null", nil}} {
		if strings.HasPrefix(p.text[p.at:], lit.word) {
			p.at += len(lit.word)
			return lit.v, nil
		}
	}
	return p.number(path)
}

func (p *parser) object(path string, depth int) (any, *stop) {
	p.at++
	out := NewObject()
	p.skip()
	if p.peek() == '}' {
		p.at++
		return out, nil
	}
	for {
		p.skip()
		if p.peek() != '"' {
			return nil, &stop{path, "JSON"}
		}
		k, s := p.string(path, true)
		if s != nil {
			return nil, s
		}
		key := k.(string)
		at := Member(path, key)
		if out.Has(key) {
			return nil, &stop{at, "unique member name"}
		}
		p.skip()
		if p.peek() != ':' {
			return nil, &stop{path, "JSON"}
		}
		p.at++
		item, s := p.value(at, depth+1)
		if s != nil {
			return nil, s
		}
		out.Set(key, item)
		p.skip()
		switch p.peek() {
		case ',':
			p.at++
			continue
		case '}':
			p.at++
			return out, nil
		}
		return nil, &stop{path, "JSON"}
	}
}

func (p *parser) array(path string, depth int) (any, *stop) {
	p.at++
	out := []any{}
	p.skip()
	if p.peek() == ']' {
		p.at++
		return out, nil
	}
	for {
		item, s := p.value(Element(path, len(out)), depth+1)
		if s != nil {
			return nil, s
		}
		out = append(out, item)
		p.skip()
		switch p.peek() {
		case ',':
			p.at++
			continue
		case ']':
			p.at++
			return out, nil
		}
		return nil, &stop{path, "JSON"}
	}
}

func hexValue(c byte) (rune, bool) {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0'), true
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10, true
	}
	return 0, false
}

// string reads the string at p.at (the opening quote). A member name passes key, so a name holding a lone
// surrogate is reported at its own path (spec §3).
func (p *parser) string(path string, key bool) (any, *stop) {
	var b strings.Builder
	lone := false
	i := p.at + 1
	start := i // start of the pending run of unescaped bytes
	for {
		if i >= len(p.text) {
			return nil, &stop{path, "JSON"}
		}
		c := p.text[i]
		if c == '"' {
			break
		}
		if c < 0x20 {
			return nil, &stop{path, "JSON"}
		}
		if c != '\\' {
			i++
			continue
		}
		b.WriteString(p.text[start:i])
		if i+1 >= len(p.text) {
			return nil, &stop{path, "JSON"}
		}
		switch e := p.text[i+1]; e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			u, ok := p.hex4(i + 2)
			if !ok {
				return nil, &stop{path, "JSON"}
			}
			i += 6
			if utf16.IsSurrogate(u) {
				if u < 0xDC00 && i+1 < len(p.text) && p.text[i] == '\\' && p.text[i+1] == 'u' {
					if low, ok := p.hex4(i + 2); ok && low >= 0xDC00 && low <= 0xDFFF {
						b.WriteRune(utf16.DecodeRune(u, low))
						i += 6
						start = i
						continue
					}
				}
				lone = true
				writeWTF8(&b, u)
			} else {
				b.WriteRune(u)
			}
			start = i
			continue
		default:
			return nil, &stop{path, "JSON"}
		}
		i += 2
		start = i
	}
	b.WriteString(p.text[start:i])
	p.at = i + 1
	s := b.String()
	if lone {
		at := path
		if key {
			at = Member(path, s)
		}
		return nil, &stop{at, "well-formed Unicode text"}
	}
	return s, nil
}

func (p *parser) hex4(at int) (rune, bool) {
	if at+4 > len(p.text) {
		return 0, false
	}
	var r rune
	for k := 0; k < 4; k++ {
		h, ok := hexValue(p.text[at+k])
		if !ok {
			return 0, false
		}
		r = r<<4 | h
	}
	return r, true
}

// writeWTF8 writes a surrogate code point as its three-byte generalized UTF-8 form, so a lone surrogate
// survives in a Go string (only ever in an error path) and Stringify can escape it back to \udxxx.
func writeWTF8(b *strings.Builder, r rune) {
	b.WriteByte(byte(0xE0 | r>>12))
	b.WriteByte(byte(0x80 | (r>>6)&0x3F))
	b.WriteByte(byte(0x80 | r&0x3F))
}

// number reads /-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/ at p.at, as core/ir/json.ts's sticky
// regex does (each optional part is taken only when complete), and converts it as Number(text) would.
func (p *parser) number(path string) (any, *stop) {
	t, i := p.text, p.at
	digits := func(j int) int {
		for j < len(t) && t[j] >= '0' && t[j] <= '9' {
			j++
		}
		return j
	}
	if i < len(t) && t[i] == '-' {
		i++
	}
	switch {
	case i < len(t) && t[i] == '0':
		i++
	case i < len(t) && t[i] >= '1' && t[i] <= '9':
		i = digits(i)
	default:
		return nil, &stop{path, "JSON"}
	}
	if i+1 < len(t) && t[i] == '.' && t[i+1] >= '0' && t[i+1] <= '9' {
		i = digits(i + 1)
	}
	if i < len(t) && (t[i] == 'e' || t[i] == 'E') {
		j := i + 1
		if j < len(t) && (t[j] == '+' || t[j] == '-') {
			j++
		}
		if j < len(t) && t[j] >= '0' && t[j] <= '9' {
			i = digits(j)
		}
	}
	f, err := strconv.ParseFloat(t[p.at:i], 64)
	// ParseFloat reports ErrRange for overflow (±Inf) and underflow (0 or a subnormal); only overflow is
	// refused, as Number("1e-400") is 0.
	if err != nil && (math.IsInf(f, 0) || math.IsNaN(f)) {
		return nil, &stop{path, "finite number"}
	}
	p.at = i
	return f, nil
}

// FormatNumber is ECMAScript Number::toString(x) (what JSON.stringify and String() write): the shortest
// round-trip digits, plain notation for exponents in [-7, 21), otherwise d.ddde±n; -0 is "0".
func FormatNumber(f float64) string {
	switch {
	case f == 0:
		return "0"
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±xx
	cut := strings.IndexByte(e, 'e')
	digits := strings.Replace(e[:cut], ".", "", 1)
	exp, _ := strconv.Atoi(e[cut+1:])
	k, n := len(digits), exp+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	es := "+"
	if n-1 < 0 {
		es = "-"
	}
	mant := digits[:1]
	if k > 1 {
		mant += "." + digits[1:]
	}
	return sign + mant + "e" + es + strconv.Itoa(abs(n-1))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Stringify is JSON.stringify(v) without indentation: members in ECMAScript order, Undefined members
// dropped (and Undefined elements written as null), non-finite numbers as null, strings escaped as
// ECMAScript does (only ", \, and control characters; lone surrogates as \udxxx).
func Stringify(v any) string {
	var b strings.Builder
	writeJSON(&b, v, "", "")
	return b.String()
}

// StringifyIndent is JSON.stringify(v, null, indent).
func StringifyIndent(v any, indent string) string {
	var b strings.Builder
	writeJSON(&b, v, indent, "")
	return b.String()
}

func writeJSON(b *strings.Builder, v any, indent, prefix string) {
	inner := prefix + indent
	open := func(c byte) {
		b.WriteByte(c)
		if indent != "" {
			b.WriteString("\n" + inner)
		}
	}
	sep := func() {
		b.WriteByte(',')
		if indent != "" {
			b.WriteString("\n" + inner)
		}
	}
	end := func(c byte) {
		if indent != "" {
			b.WriteString("\n" + prefix)
		}
		b.WriteByte(c)
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			b.WriteString("null")
		} else {
			b.WriteString(FormatNumber(x))
		}
	case string:
		quote(b, x)
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		open('[')
		for i, item := range x {
			if i > 0 {
				sep()
			}
			if item == Undefined {
				item = nil
			}
			writeJSON(b, item, indent, inner)
		}
		end(']')
	case *Object:
		if x.Len() == 0 {
			b.WriteString("{}")
			return
		}
		open('{')
		for i, k := range x.keys {
			if i > 0 {
				sep()
			}
			quote(b, k)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			writeJSON(b, x.vals[k], indent, inner)
		}
		end('}')
	default:
		b.WriteString("null")
	}
}

const hexDigits = "0123456789abcdef"

// quote writes s as an ECMAScript JSON string literal. Bytes that encode a surrogate code point (only
// produced by Parse for a lone surrogate) are written as \udxxx, as JSON.stringify writes a lone surrogate.
func quote(b *strings.Builder, s string) {
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c != 0xED {
			if c < utf8.RuneSelf {
				i++
				continue
			}
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
			continue
		}
		var esc string
		size := 1
		switch c {
		case '"':
			esc = `\"`
		case '\\':
			esc = `\\`
		case '\b':
			esc = `\b`
		case '\f':
			esc = `\f`
		case '\n':
			esc = `\n`
		case '\r':
			esc = `\r`
		case '\t':
			esc = `\t`
		case 0xED:
			if i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF {
				r := rune(c&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
				esc = `\u` + string([]byte{hexDigits[r>>12], hexDigits[r>>8&0xF], hexDigits[r>>4&0xF], hexDigits[r&0xF]})
				size = 3
			} else {
				_, size = utf8.DecodeRuneInString(s[i:])
				i += size
				continue
			}
		default:
			esc = `\u00` + string([]byte{hexDigits[c>>4], hexDigits[c&0xF]})
		}
		b.WriteString(s[start:i])
		b.WriteString(esc)
		i += size
		start = i
	}
	b.WriteString(s[start:])
	b.WriteByte('"')
}

// Canonical is the RFC 8785 (JCS) encoding of v (spec §9, core/ir/json.ts canonical): no whitespace,
// object members sorted by the UTF-16 code units of their names, numbers and strings as JSON.stringify
// writes them, Undefined members dropped.
func Canonical(v any) string {
	var b strings.Builder
	writeCanonical(&b, v)
	return b.String()
}

func writeCanonical(b *strings.Builder, v any) {
	switch x := v.(type) {
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if item == Undefined {
				item = nil
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')
	case *Object:
		keys := append([]string(nil), x.keys...)
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			quote(b, k)
			b.WriteByte(':')
			writeCanonical(b, x.vals[k])
		}
		b.WriteByte('}')
	default:
		writeJSON(b, v, "", "")
	}
}

// utf16Less orders strings by their UTF-16 code units, as JavaScript's default sort does. It differs from
// byte order only when a supplementary character (surrogate pair, 0xD800–0xDBFF) meets a BMP character in
// 0xE000–0xFFFF.
func utf16Less(a, b string) bool {
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			ua, ub := firstUnit(ra), firstUnit(rb)
			if ua != ub {
				return ua < ub
			}
			return ra < rb // same high surrogate: the low surrogates order as the code points do
		}
		a, b = a[na:], b[nb:]
	}
	return len(a) < len(b)
}

func firstUnit(r rune) rune {
	if r >= 0x10000 {
		hi, _ := utf16.EncodeRune(r)
		return hi
	}
	return r
}

// ToString is JavaScript's String(v) for a JSON value: arrays join their elements with commas (null and
// undefined as empty), objects are "[object Object]".
func ToString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return FormatNumber(x)
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			if item != nil && item != Undefined {
				parts[i] = ToString(item)
			}
		}
		return strings.Join(parts, ",")
	case *Object:
		return "[object Object]"
	}
	return "undefined"
}
