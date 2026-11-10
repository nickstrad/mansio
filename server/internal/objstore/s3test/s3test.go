// Package s3test is an in-process, misbehaving S3 server for tests (Release It guide H4, plan RI-13). It
// speaks enough path-style S3 for internal/objstore: object GET, HEAD, PUT (If-Match / If-None-Match,
// Content-MD5, x-amz-meta-*), DELETE, ListObjectsV2, multi-object delete, bucket HEAD/PUT/location.
// Conditional PUTs are checked and applied under one lock, the semantics AWS documents.
//
// It also breaks the protocol on request: stall after the headers, dribble the body, truncate it, lie in
// Content-Length, answer 5xx/429, apply a PUT and drop the reply (or answer 412), corrupt the body. Each fault is a
// counter-limited switch, so a test can fail N requests and then see the server heal.
//
// It is NOT evidence about a real backend: that is TestS3Compat's job.
package s3test

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BucketState is what bucket HEAD and PUT (create) answer for one bucket. Object requests ignore it.
type BucketState int

const (
	// BucketOwned (every bucket, unless SetBucket says otherwise): it exists and is the caller's. HEAD 200;
	// PUT 409 BucketAlreadyOwnedByYou, as S3 answers outside us-east-1 and SeaweedFS everywhere.
	BucketOwned BucketState = iota
	// BucketMissing: HEAD 404; PUT creates it (200), and it is BucketOwned from then on.
	BucketMissing
	// BucketCreatedSinceHead: HEAD 404, PUT 409 BucketAlreadyOwnedByYou: what the loser of two creators of one
	// account sees (the other's PUT landed between its HEAD and its PUT).
	BucketCreatedSinceHead
	// BucketTakenSinceHead: HEAD 404, PUT 409 BucketAlreadyExists: another account created the name in between.
	BucketTakenSinceHead
	// BucketForeign: another account's bucket. HEAD 403; PUT 409 BucketAlreadyExists.
	BucketForeign
)

// Mode is one protocol fault.
type Mode int

const (
	Healthy            Mode = iota
	HeadersThenStall        // GET/HEAD (a listing too): send the headers, then nothing until the client gives up
	Dribble                 // GET: send the body at Rate bytes/s (default 1)
	TruncateBody            // GET: declare the full length, send half, close the connection
	WrongContentLength      // GET: declare and send one byte less than the object holds
	Status                  // any request: answer with Status (and Code) instead
	ApplyThenDropReply      // PUT: apply the write, then close the connection without a response
	CorruptBody             // GET: flip one byte of the body
	// ApplyThenPreconditionFailed: PUT: apply the write, then answer 412. That is what a client that re-sends
	// a conditional PUT whose reply was lost sees (minio-go's own retries did this, object-storage plan G2).
	ApplyThenPreconditionFailed
)

func (m Mode) String() string {
	return [...]string{"healthy", "headers_then_stall", "dribble", "truncate_body", "wrong_content_length",
		"status", "apply_then_drop_reply", "corrupt_body", "apply_then_precondition_failed"}[m]
}

// Fault is a mode, which requests it hits, and how many times.
type Fault struct {
	Mode   Mode
	Method string // only requests with this method ("" = any the mode applies to)
	Times  int    // matching requests that get it; <= 0 = every one until Heal
	Status int    // Status: the HTTP status (default 503)
	Code   string // Status: the S3 error code (default from the status: SlowDown, InternalError, ...)
	Rate   int    // Dribble: bytes per second (default 1)
}

// Request is one request the server saw (bucket location lookups excluded).
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Fault  Mode // the fault it got, Healthy if none
}

type object struct {
	data []byte
	meta http.Header // X-Amz-Meta-* and Content-Type
	mod  time.Time
}

// Server is safe for concurrent use.
type Server struct {
	srv  *httptest.Server
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	objects map[string]object // "/bucket/key" → object
	buckets map[string]BucketState
	fault   Fault
	left    int
	reqs    []Request
}

// New starts a server; Close stops it.
func New() *Server { return start(httptest.NewServer) }

// NewTLS starts a server that speaks HTTPS with a self-signed certificate; CAPEM is the CA to trust.
func NewTLS() *Server { return start(httptest.NewTLSServer) }

func start(serve func(http.Handler) *httptest.Server) *Server {
	s := &Server{objects: map[string]object{}, done: make(chan struct{})}
	s.srv = serve(s)
	return s
}

// Endpoint is the server's host:port, for objstore.Config.Endpoint.
func (s *Server) Endpoint() string { return s.srv.Listener.Addr().String() }

// CAPEM is the PEM certificate a client must trust to reach a NewTLS server (nil for plain HTTP).
func (s *Server) CAPEM() []byte {
	if s.srv.Certificate() == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw})
}

// Close releases stalled handlers, then stops the server.
func (s *Server) Close() {
	s.once.Do(func() { close(s.done) })
	s.srv.CloseClientConnections()
	s.srv.Close()
}

// SetBucket sets what bucket HEAD and PUT answer for bucket (default: BucketOwned).
func (s *Server) SetBucket(bucket string, st BucketState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets == nil {
		s.buckets = map[string]BucketState{}
	}
	s.buckets[bucket] = st
}

// Inject sets the fault for the next matching requests (replacing any previous one).
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Mode == Status && f.Status == 0 {
		f.Status = http.StatusServiceUnavailable
	}
	if f.Mode == Dribble && f.Rate <= 0 {
		f.Rate = 1
	}
	s.fault, s.left = f, max(f.Times, 0)
}

// Heal clears the fault.
func (s *Server) Heal() { s.Inject(Fault{}) }

// Requests returns every request seen so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Count is how many requests with method were seen ("" = all).
func (s *Server) Count(method string) int {
	n := 0
	for _, r := range s.Requests() {
		if method == "" || r.Method == method {
			n++
		}
	}
	return n
}

// ResetRequests forgets the requests seen so far.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = nil
}

// Object returns the bytes stored at bucket/key.
func (s *Server) Object(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects["/"+bucket+"/"+key]
	return bytes.Clone(o.data), ok
}

// Put stores an object directly (test setup; no faults apply).
func (s *Server) Put(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects["/"+bucket+"/"+key] = object{data: bytes.Clone(data), meta: http.Header{}, mod: time.Now().UTC()}
}

// ETag is the S3 ETag of a single-PUT object: hex MD5 of its bytes.
func ETag(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }

// applies says whether mode m can act on a request with this method.
func applies(m Mode, method string) bool {
	switch m {
	case Status:
		return true
	case HeadersThenStall:
		return method == http.MethodGet || method == http.MethodHead
	case ApplyThenDropReply, ApplyThenPreconditionFailed:
		return method == http.MethodPut
	case Dribble, TruncateBody, WrongContentLength, CorruptBody:
		return method == http.MethodGet
	}
	return false
}

// take returns the fault this request gets and records the request. Called with s.mu held.
func (s *Server) take(r *http.Request, objectReq bool) Fault {
	f := s.fault
	got := Fault{}
	if f.Mode != Healthy && objectReq && (f.Method == "" || f.Method == r.Method) && applies(f.Mode, r.Method) {
		got = f
		if s.left > 0 {
			if s.left--; s.left == 0 {
				s.fault = Fault{}
			}
		}
	}
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Header: r.Header.Clone(), Fault: got.Mode})
	return got
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, ok := q["location"]; ok {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	path := "/" + bucket + "/" + key

	// Read a PUT/POST body before taking the lock: a slow client must not block everyone.
	var body []byte
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			writeError(w, r, http.StatusBadRequest, "IncompleteBody")
			return
		}
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			if body, err = dechunk(body); err != nil {
				writeError(w, r, http.StatusBadRequest, "IncompleteBody")
				return
			}
		}
	}

	s.mu.Lock()
	f := s.take(r, key != "" || q.Has("list-type") || q.Has("delete"))
	if f.Mode == Status {
		s.mu.Unlock()
		writeError(w, r, f.Status, f.Code)
		return
	}
	if key == "" {
		if f.Mode == HeadersThenStall { // a listing whose status line and headers arrive and whose body never does
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			s.wait(r, time.Duration(1<<62))
			return
		}
		s.bucketOp(w, r, bucket, body) // unlocks
		return
	}
	cur, exists := s.objects[path]
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.mu.Unlock()
		if !exists {
			writeError(w, r, http.StatusNotFound, "NoSuchKey")
			return
		}
		s.serveObject(w, r, cur, f)
	case http.MethodPut:
		status, code, tag := s.putLocked(r, path, cur, exists, body)
		s.mu.Unlock()
		switch {
		case f.Mode == ApplyThenDropReply:
			dropConnection(w)
			return
		case f.Mode == ApplyThenPreconditionFailed && status == http.StatusOK:
			writeError(w, r, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if status != http.StatusOK {
			writeError(w, r, status, code)
			return
		}
		w.Header().Set("ETag", `"`+tag+`"`)
	case http.MethodDelete:
		delete(s.objects, path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s.mu.Unlock()
		writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// putLocked checks the PUT's conditions and Content-MD5 and applies it. Called with s.mu held.
func (s *Server) putLocked(r *http.Request, path string, cur object, exists bool, body []byte) (status int, code, tag string) {
	if want := r.Header.Get("Content-Md5"); want != "" {
		h := md5.Sum(body)
		if base64.StdEncoding.EncodeToString(h[:]) != want {
			return http.StatusBadRequest, "BadDigest", ""
		}
	}
	if r.Header.Get("If-None-Match") == "*" && exists {
		return http.StatusPreconditionFailed, "PreconditionFailed", ""
	}
	if im := r.Header.Get("If-Match"); im != "" && (!exists || strings.Trim(im, `"`) != ETag(cur.data)) {
		return http.StatusPreconditionFailed, "PreconditionFailed", ""
	}
	meta := http.Header{}
	for k, v := range r.Header {
		if strings.HasPrefix(k, "X-Amz-Meta-") || k == "Content-Type" {
			meta[k] = v
		}
	}
	s.objects[path] = object{data: body, meta: meta, mod: time.Now().UTC()}
	return http.StatusOK, "", ETag(body)
}

func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, o object, f Fault) {
	data := o.data
	h := w.Header()
	for k, v := range o.meta {
		h[k] = v
	}
	h.Set("ETag", `"`+ETag(data)+`"`)
	h.Set("Last-Modified", o.mod.Format(http.TimeFormat))
	switch f.Mode {
	case WrongContentLength:
		data = data[:max(len(data)-1, 0)]
	case CorruptBody:
		if len(data) > 0 {
			data = bytes.Clone(data)
			data[len(data)/2] ^= 0xff
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		if f.Mode == HeadersThenStall {
			// a HEAD has no body to stall in: hold the response back instead
			s.wait(r, time.Duration(1<<62))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	switch f.Mode {
	case HeadersThenStall:
		fl.Flush()
		s.wait(r, time.Duration(1<<62))
	case TruncateBody:
		w.Write(data[:len(data)/2])
		fl.Flush()
		dropConnection(w)
	case Dribble:
		every := time.Second / time.Duration(f.Rate)
		for i := range data {
			w.Write(data[i : i+1])
			fl.Flush()
			if !s.wait(r, every) {
				return
			}
		}
	default:
		w.Write(data)
	}
}

// wait sleeps d, or until the client goes away or the server closes; it reports whether d passed.
func (s *Server) wait(r *http.Request, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
	case <-s.done:
	}
	return false
}

func dropConnection(w http.ResponseWriter) {
	if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
		c.Close()
	}
}

// bucketOp serves bucket-level requests. A bucket exists and is the caller's unless SetBucket said otherwise
// (which only HEAD and PUT look at). Called with s.mu held; unlocks it.
func (s *Server) bucketOp(w http.ResponseWriter, r *http.Request, bucket string, body []byte) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		res := s.listLocked(bucket, q)
		s.mu.Unlock()
		writeXML(w, res)
	case r.Method == http.MethodPost && q.Has("delete"):
		var req struct {
			Objects []struct{ Key string } `xml:"Object"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			s.mu.Unlock()
			writeError(w, r, http.StatusBadRequest, "MalformedXML")
			return
		}
		type deleted struct{ Key string }
		var res struct {
			XMLName xml.Name  `xml:"DeleteResult"`
			Deleted []deleted `xml:"Deleted"`
		}
		for _, o := range req.Objects {
			delete(s.objects, "/"+bucket+"/"+o.Key)
			res.Deleted = append(res.Deleted, deleted{o.Key})
		}
		s.mu.Unlock()
		writeXML(w, res)
	case r.Method == http.MethodHead || r.Method == http.MethodPut:
		st := s.buckets[bucket]
		if st == BucketMissing && r.Method == http.MethodPut {
			delete(s.buckets, bucket)
		}
		s.mu.Unlock()
		put := r.Method == http.MethodPut
		switch {
		case st == BucketMissing && put:
		case st == BucketForeign && !put:
			writeError(w, r, http.StatusForbidden, "AccessDenied")
		case st != BucketOwned && !put:
			writeError(w, r, http.StatusNotFound, "NoSuchBucket")
		case st == BucketOwned || st == BucketCreatedSinceHead:
			if put {
				writeError(w, r, http.StatusConflict, "BucketAlreadyOwnedByYou")
			}
		default:
			writeError(w, r, http.StatusConflict, "BucketAlreadyExists")
		}
	default:
		s.mu.Unlock()
		writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

type listContent struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string
	Prefix                string
	Delimiter             string `xml:",omitempty"`
	KeyCount              int
	MaxKeys               int
	IsTruncated           bool
	ContinuationToken     string `xml:",omitempty"`
	NextContinuationToken string `xml:",omitempty"`
	Contents              []listContent
	CommonPrefixes        []commonPrefix
}

type commonPrefix struct{ Prefix string }

// listLocked is one ListObjectsV2 page, in key order. With a delimiter, keys below it fold into common
// prefixes, which count toward max-keys and can be a continuation token. Called with s.mu held.
func (s *Server) listLocked(bucket string, q url.Values) listResult {
	get := q.Get
	prefix, delim, after := get("prefix"), get("delimiter"), max(get("start-after"), get("continuation-token"))
	maxKeys := 1000
	if n, err := strconv.Atoi(get("max-keys")); err == nil && n > 0 && n < maxKeys {
		maxKeys = n
	}
	folded := map[string]bool{} // entry → is a common prefix
	for p := range s.objects {
		k, ok := strings.CutPrefix(p, "/"+bucket+"/")
		if !ok || !strings.HasPrefix(k, prefix) {
			continue
		}
		if i := strings.Index(k[len(prefix):], delim); delim != "" && i >= 0 {
			folded[k[:len(prefix)+i+len(delim)]] = true
		} else {
			folded[k] = false
		}
	}
	var entries []string
	for e := range folded {
		if e > after {
			entries = append(entries, e)
		}
	}
	sort.Strings(entries)
	res := listResult{Name: bucket, Prefix: prefix, Delimiter: delim, MaxKeys: maxKeys, ContinuationToken: get("continuation-token")}
	if len(entries) > maxKeys {
		entries, res.IsTruncated = entries[:maxKeys], true
		res.NextContinuationToken = entries[maxKeys-1]
	}
	for _, e := range entries {
		if folded[e] {
			res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{e})
			continue
		}
		o := s.objects["/"+bucket+"/"+e]
		res.Contents = append(res.Contents, listContent{Key: e, LastModified: o.mod.Format(time.RFC3339),
			ETag: `"` + ETag(o.data) + `"`, Size: int64(len(o.data)), StorageClass: "STANDARD"})
	}
	res.KeyCount = len(entries)
	return res
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	b, _ := xml.Marshal(v)
	w.Write(append([]byte(xml.Header), b...))
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string) {
	if code == "" {
		switch {
		case status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests:
			code = "SlowDown"
		case status >= 500:
			code = "InternalError"
		default:
			code = http.StatusText(status)
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
	}
}

// dechunk decodes an aws-chunked body ("<hex size>[;chunk-signature=...]\r\n<data>\r\n" ... "0...\r\n",
// then optional trailer lines, which are dropped).
func dechunk(b []byte) ([]byte, error) {
	rd := bufio.NewReader(bytes.NewReader(b))
	var out []byte
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		var n int
		if _, err := fmt.Sscanf(strings.SplitN(strings.TrimSpace(line), ";", 2)[0], "%x", &n); err != nil {
			return nil, err
		}
		if n == 0 {
			return out, nil
		}
		chunk := make([]byte, n+2)
		if _, err := io.ReadFull(rd, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk[:n]...)
	}
}
