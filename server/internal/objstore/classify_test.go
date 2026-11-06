package objstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"syscall"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/objstore"
)

// Spec table for Classify, one row per partition, with on/off points at the status boundaries
// (408/409, 412, 429, 499/500, 503) and the codes that override the status.
func TestClassify(t *testing.T) {
	resp := func(status int, code string) error { return minio.ErrorResponse{StatusCode: status, Code: code} }
	wrapped := func(err error) error { return errs.Wrap("x", fmt.Errorf("ctx: %w", err), "outer") }
	for _, tc := range []struct {
		name string
		err  error
		want objstore.Kind
	}{
		{"nil", nil, objstore.OK},
		{"400 bad request", resp(400, "BadRequest"), objstore.Permanent},
		{"403 AccessDenied", resp(403, "AccessDenied"), objstore.Permanent},
		{"400 BadDigest", resp(400, "BadDigest"), objstore.Permanent},
		{"404 NoSuchKey", resp(404, "NoSuchKey"), objstore.NotFound},
		{"404 no code", resp(404, ""), objstore.NotFound},
		{"NoSuchKey without status", resp(0, "NoSuchKey"), objstore.NotFound},
		{"408 request timeout (on point below 409)", resp(408, ""), objstore.Transient},
		{"409 conflict (off point above 408)", resp(409, ""), objstore.Precondition},
		{"409 ConditionalRequestConflict", resp(409, "ConditionalRequestConflict"), objstore.Precondition},
		{"412 PreconditionFailed", resp(412, "PreconditionFailed"), objstore.Precondition},
		{"428 (just below 429)", resp(428, ""), objstore.Permanent},
		{"429 too many requests", resp(429, ""), objstore.Transient},
		{"499 (last 4xx)", resp(499, ""), objstore.Permanent},
		{"500 (first 5xx)", resp(500, "InternalError"), objstore.Transient},
		{"503 SlowDown", resp(503, "SlowDown"), objstore.Transient},
		{"SlowDown code under a 400", resp(400, "SlowDown"), objstore.Transient},
		{"RequestTimeout code", resp(400, "RequestTimeout"), objstore.Transient},
		{"local minio validation error", resp(0, "InvalidArgument"), objstore.Permanent},
		{"wrapped 503", wrapped(resp(503, "SlowDown")), objstore.Transient},
		{"context canceled", context.Canceled, objstore.Canceled},
		{"context deadline", wrapped(context.DeadlineExceeded), objstore.Canceled},
		{"our own timeout beats the ctx error it carries", errors.Join(objstore.ErrTimeout, context.Canceled), objstore.Transient},
		{"url error (connection closed)", &url.Error{Op: "Get", URL: "u", Err: errors.New("Connection closed by foreign host")}, objstore.Transient},
		{"connection refused", wrapped(syscall.ECONNREFUSED), objstore.Transient},
		{"connection reset", syscall.ECONNRESET, objstore.Transient},
		{"truncated body", wrapped(io.ErrUnexpectedEOF), objstore.Transient},
		{"durable.ErrStale", wrapped(durable.ErrStale), objstore.Precondition},
		{"durable.ErrExists", durable.ErrExists, objstore.Precondition},
		{"durable.ErrNoManifest", durable.ErrNoManifest, objstore.NotFound},
		{"durable.ErrNotFound", durable.ErrNotFound, objstore.NotFound},
		{"objstore.ErrNotFound", objstore.ErrNotFound, objstore.NotFound},
		{"durable.ErrCorrupt", durable.ErrCorrupt, objstore.Permanent},
		{"plain local error", errors.New("open x: permission denied"), objstore.Permanent},
	} {
		if got := objstore.Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
