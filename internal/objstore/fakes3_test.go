package objstore_test

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is a minimal path-style S3 server: GET, HEAD, PUT of whole objects, with If-Match / If-None-Match
// on PUT checked and applied under one lock (the semantics AWS documents). It lets the adapter's request
// logic (which headers it sends, how it maps 412) run without Docker. It is NOT evidence about SeaweedFS.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []http.Header
}

func newFakeS3(t *testing.T) (*fakeS3, string) {
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, strings.TrimPrefix(srv.URL, "http://")
}

func etag(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["location"]; ok {
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
		return
	}
	key := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, exists := f.objects[key]
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !exists {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", `"`+etag(cur)+`"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(cur)))
		w.Header().Set("Last-Modified", "Thu, 05 Nov 2026 12:00:00 GMT")
		if r.Method == http.MethodGet {
			w.Write(cur)
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		hdr := r.Header.Clone()
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			if body, err = dechunk(body, hdr); err != nil {
				s3Error(w, http.StatusBadRequest, "IncompleteBody")
				return
			}
		}
		f.puts = append(f.puts, hdr)
		if inm := r.Header.Get("If-None-Match"); inm == "*" && exists {
			s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if im := r.Header.Get("If-Match"); im != "" && (!exists || strings.Trim(im, `"`) != etag(cur)) {
			s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		f.objects[key] = body
		w.Header().Set("ETag", `"`+etag(body)+`"`)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// dechunk decodes an aws-chunked body ("<hex size>[;chunk-signature=...]\r\n<data>\r\n" ... "0...\r\n"
// then optional trailer lines) and copies trailers (e.g. x-amz-checksum-sha256) into hdr.
func dechunk(b []byte, hdr http.Header) ([]byte, error) {
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
			break
		}
		chunk := make([]byte, n+2)
		if _, err := io.ReadFull(rd, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk[:n]...)
	}
	for {
		line, err := rd.ReadString('\n')
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			hdr.Set(k, v)
		}
		if err != nil {
			return out, nil
		}
	}
}
