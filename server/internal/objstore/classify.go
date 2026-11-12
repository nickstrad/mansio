package objstore

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"

	"github.com/minio/minio-go/v7"

	"github.com/nickstrad/mansio/server/internal/durable"
)

// Kind is what an S3 error means for the caller. Callers branch on it, never on message text. Errors leaving this
// package carry it as the "s3_kind" Misc field too.
type Kind int

const (
	OK           Kind = iota // no error
	Transient                // 5xx, 429, SlowDown, network error, our own timeout: retry (idempotent requests only)
	Precondition             // 412 or 409, or durable.ErrStale / ErrExists: do not retry blindly; re-read
	NotFound                 // 404, NoSuchKey, durable.ErrNoManifest / ErrNotFound: do not retry
	Permanent                // other 4xx (AccessDenied, BadDigest, ...), durable.ErrCorrupt, local errors: do not retry
	Canceled                 // the caller's context ended: do not retry
)

func (k Kind) String() string {
	return [...]string{"ok", "transient", "precondition", "not_found", "permanent", "canceled"}[k]
}

// ErrTimeout matches (errors.Is) a request that hit this package's own deadline: the per-operation timeout,
// the size-derived transfer deadline, or the stall timeout. It is Transient. The caller's own context
// ending is Canceled instead.
var ErrTimeout = errors.New("objstore: S3 request timed out")

// Classify says what err means. It looks through wrapping (errs.Error, errors.Join, *url.Error).
//
//	post: Classify(nil) == OK; every other error gets exactly one of the five other kinds
func Classify(err error) Kind {
	if err == nil {
		return OK
	}
	switch {
	case errors.Is(err, ErrTimeout):
		return Transient
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Canceled
	case errors.Is(err, durable.ErrStale), errors.Is(err, durable.ErrExists):
		return Precondition
	case errors.Is(err, durable.ErrNoManifest), errors.Is(err, durable.ErrNotFound), errors.Is(err, ErrNotFound):
		return NotFound
	case errors.Is(err, durable.ErrCorrupt):
		return Permanent
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.StatusCode != 0 || resp.Code != "") {
		return classifyResponse(resp)
	}
	var ne net.Error
	var ue *url.Error
	if errors.As(err, &ne) || errors.As(err, &ue) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return Transient
	}
	return Permanent
}

// noSuchKey reports whether err is S3's answer for a missing object (not a missing bucket).
func noSuchKey(err error) bool {
	var r minio.ErrorResponse
	return errors.As(err, &r) && (r.Code == minio.NoSuchKey || r.Code == "" && r.StatusCode == http.StatusNotFound)
}

func classifyResponse(r minio.ErrorResponse) Kind {
	switch r.Code {
	case "SlowDown", "RequestTimeout", "InternalError", "ServiceUnavailable":
		return Transient
	case minio.PreconditionFailed, "ConditionalRequestConflict":
		return Precondition
	case minio.NoSuchKey, minio.NoSuchBucket:
		return NotFound
	}
	switch s := r.StatusCode; {
	case s == http.StatusPreconditionFailed, s == http.StatusConflict:
		return Precondition
	case s == http.StatusNotFound:
		return NotFound
	case s == http.StatusRequestTimeout, s == http.StatusTooManyRequests, s >= 500:
		return Transient
	}
	return Permanent
}
