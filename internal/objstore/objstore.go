// Package objstore is the S3 adapter (minio-go) for moving SQLite files to and from object storage
// (SeaweedFS in the coldload test). Errors leaving it are errs.Error with module "objstore", carrying the
// Classify kind as Misc "s3_kind"; a missing object matches ErrNotFound.
//
// It owns the S3 call budget (Release It T1–T4, R1–R2): minio-go's retries are off, idempotent requests (GET,
// HEAD, LIST, DELETE) are retried here with jittered backoff, writes are sent once, every request has a
// deadline, and bodies have a stall timeout plus a size-derived deadline.
package objstore

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/nickstrad/ts-db-go/internal/errs"
)

const module = "objstore"

// ErrNotFound matches (errors.Is) a GET of an object that does not exist.
var ErrNotFound = errors.New("objstore: object not found")

// Config says where the store is. Endpoint, AccessKey, SecretKey and Bucket are required; the rest have
// defaults (Release It guide §P, S3 sheet).
type Config struct {
	Endpoint  string // host:port, e.g. localhost:8333
	AccessKey string
	SecretKey string
	Bucket    string
	// MaxConns caps open connections per host; set it to at least the fetch concurrency, or fetches queue
	// for a socket. Default 1024.
	MaxConns int
	// OpTimeout bounds a small request (manifest GET, HEAD, small PUT, one LIST page) and the wait for
	// response headers. Default 5 s.
	OpTimeout time.Duration
	// StallTimeout fails a transfer that moves no byte for this long. Default 10 s.
	StallTimeout time.Duration
	// MinThroughput (bytes/s) sizes a transfer's deadline: OpTimeout + size/MinThroughput. Default 32 MiB/s.
	MinThroughput int64
	// Logger gets one WARN line per retried request (S3.REQUEST_RETRIED). Default: discard.
	Logger *slog.Logger
}

// normalize fills the defaults.
func (c *Config) normalize() {
	if c.MaxConns <= 0 {
		c.MaxConns = 1024
	}
	if c.OpTimeout <= 0 {
		c.OpTimeout = 5 * time.Second
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = 10 * time.Second
	}
	if c.MinThroughput <= 0 {
		c.MinThroughput = 32 << 20
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

// Store is safe for concurrent use.
type Store struct {
	c       *minio.Client
	core    minio.Core
	bucket  string
	cfg     Config
	metrics metrics
	bufs    chan []byte // free list of copy buffers; survives GC, unlike sync.Pool
}

const (
	copyBuf  = 1 << 20
	keptBufs = 64 // at most 64 MiB of idle buffers
)

func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, errs.Wrap(module, nil, "cannot create S3 client: endpoint, access key, secret key and bucket are all required")
	}
	cfg.normalize()
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          cfg.MaxConns,
		MaxIdleConnsPerHost:   cfg.MaxConns,
		MaxConnsPerHost:       cfg.MaxConns,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: cfg.OpTimeout,
		DisableCompression:    true,
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:      credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Transport:  tr,
		MaxRetries: 1, // minio-go sends each request once: this package is the one retry layer
	})
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot create S3 client for %s", cfg.Endpoint)
	}
	return &Store{c: c, core: minio.Core{Client: c}, bucket: cfg.Bucket, cfg: cfg,
		metrics: newMetrics(), bufs: make(chan []byte, keptBufs)}, nil
}

// wrap is errs.Wrap for this module, tagged with err's Classify kind.
func wrap(err error, format string, args ...any) *errs.Error {
	e := errs.Wrap(module, err, format, args...)
	if err != nil {
		e.With("s3_kind", Classify(err).String())
	}
	return e
}

// EnsureBucket creates the bucket if it does not exist.
func (s *Store) EnsureBucket(ctx context.Context) error {
	var ok bool
	err := s.retryIdempotent(ctx, OpBucket, s.bucket, s.cfg.OpTimeout, func(a *attempt) error {
		var err error
		ok, err = s.c.BucketExists(a.ctx, s.bucket)
		return err
	})
	if err != nil {
		return wrap(err, "cannot check bucket %s (is the store up?)", s.bucket)
	}
	if ok {
		return nil
	}
	err = s.sendOnce(ctx, OpBucket, s.bucket, s.cfg.OpTimeout, func(a *attempt) error {
		return s.c.MakeBucket(a.ctx, s.bucket, minio.MakeBucketOptions{})
	})
	if err != nil {
		return wrap(err, "cannot create bucket %s; safe to retry", s.bucket)
	}
	return nil
}

// PutFile uploads the local file at path as key and returns the bytes sent. It is not retried here.
func (s *Store) PutFile(ctx context.Context, key, path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, wrap(err, "cannot upload %s as %s; nothing uploaded", path, key)
	}
	var info minio.UploadInfo
	err = s.sendOnce(ctx, OpPut, key, s.transferTime(fi.Size()), func(a *attempt) error {
		var err error
		info, err = s.c.FPutObject(a.ctx, s.bucket, key, path, minio.PutObjectOptions{
			ContentType: "application/vnd.sqlite3",
			PartSize:    64 << 20,
		})
		return err
	})
	if err != nil {
		return 0, wrap(err, "cannot upload %s as %s; safe to retry", path, key)
	}
	s.moved(OpPut, info.Size)
	return info.Size, nil
}

// Size returns the size of object key (one HEAD, retried while Transient). A missing object matches
// ErrNotFound.
func (s *Store) Size(ctx context.Context, key string) (int64, error) {
	var size int64
	err := s.retryIdempotent(ctx, OpHead, key, s.cfg.OpTimeout, func(a *attempt) error {
		st, err := s.c.StatObject(a.ctx, s.bucket, key, minio.StatObjectOptions{})
		size = st.Size
		return err
	})
	if err != nil {
		if noSuchKey(err) {
			err = errors.Join(ErrNotFound, err)
		}
		return 0, wrap(err, "cannot stat %s; nothing changed, safe to retry", key).With("bucket", s.bucket)
	}
	return size, nil
}

// FetchStats splits a download into time to first byte and the rest.
type FetchStats struct {
	Bytes int64
	TTFB  time.Duration // GET sent → response headers (last attempt)
	Total time.Duration // first GET sent → local file closed (not fsynced), retries included
}

// GetToFile downloads key to path with one GET per attempt (retried while Transient).
//
//	post: err == nil ⇒ path holds exactly the object's bytes (length checked against Content-Length)
//	post: err != nil ⇒ neither path nor path+".part" exists (unless path existed before and the GET failed)
//	post: a missing object matches errors.Is(err, ErrNotFound)
//
// The file is not fsynced: it is a local cache.
func (s *Store) GetToFile(ctx context.Context, key, path string) (FetchStats, error) {
	t0 := time.Now()
	var st FetchStats
	err := s.retryIdempotent(ctx, OpGet, key, s.cfg.OpTimeout, func(a *attempt) error {
		n, size, ttfb, err := s.getToPart(a, key, path, nil)
		st.TTFB = ttfb
		if err == nil && size >= 0 && n != size {
			err = fmt.Errorf("got %d of %d bytes: %w", n, size, io.ErrUnexpectedEOF)
		}
		if err = finishPart(path, err); err == nil {
			st.Bytes = n
		}
		return err
	})
	if err != nil {
		if noSuchKey(err) {
			err = errors.Join(ErrNotFound, err)
		}
		return st, wrap(err, "cannot download %s to %s; nothing kept, safe to retry", key, path).With("bucket", s.bucket)
	}
	st.Total = time.Since(t0)
	s.moved(OpGet, st.Bytes)
	return st, nil
}

// getToPart is the body of one download attempt: GET name, extend the attempt's deadline to the size the
// headers announce (OpTimeout + size/MinThroughput), and copy the body to path+".part" under the stall
// timeout, feeding h too if it is not nil. It returns the bytes written, the announced size (-1 unknown) and
// the time to first byte. The caller checks the result and calls finishPart.
func (s *Store) getToPart(a *attempt, name, path string, h hash.Hash) (n, size int64, ttfb time.Duration, err error) {
	body, info, _, err := s.core.GetObject(a.ctx, s.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return 0, -1, 0, err
	}
	defer body.Close()
	ttfb = time.Since(a.start)
	if info.Size >= 0 {
		a.within(s.transferTime(info.Size))
	}
	n, err = s.writeFile(path+".part", a.watch(body, -1, s.cfg.StallTimeout), h)
	return n, info.Size, ttfb, err
}

// finishPart renames path+".part" to path if err is nil, and removes the .part otherwise.
func finishPart(path string, err error) error {
	if err == nil {
		err = os.Rename(path+".part", path)
	}
	if err != nil {
		os.Remove(path + ".part")
	}
	return err
}

// writeFile copies r into a new file at path through a pooled 1 MiB buffer, feeding h too if it is not nil.
// A plain read/write loop on purpose: io.CopyBuffer would hand the copy to (*os.File).ReadFrom, which
// ignores the buffer.
func (s *Store) writeFile(path string, r io.Reader, h hash.Hash) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	buf := s.getBuf()
	defer s.putBuf(buf)
	var n int64
	for {
		m, rerr := fill(r, buf)
		if m > 0 {
			if h != nil {
				h.Write(buf[:m])
			}
			if _, werr := f.Write(buf[:m]); werr != nil {
				f.Close()
				return n, werr
			}
			n += int64(m)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return n, rerr
		}
	}
	return n, f.Close()
}

// md5File returns the base64 MD5 of f's contents (a Content-MD5 value) and rewinds f.
func (s *Store) md5File(f *os.File) (string, error) {
	h := md5.New()
	buf := s.getBuf()
	defer s.putBuf(buf)
	if _, err := io.CopyBuffer(struct{ io.Writer }{h}, struct{ io.Reader }{f}, buf); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// fill reads into buf until it is full or r fails; io.EOF is the clean end. Unlike io.ReadFull it keeps a
// transport's io.ErrUnexpectedEOF (a truncated body) apart from a short final chunk.
func fill(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *Store) getBuf() []byte {
	select {
	case b := <-s.bufs:
		return b
	default:
		return make([]byte, copyBuf)
	}
}

func (s *Store) putBuf(b []byte) {
	select {
	case s.bufs <- b:
	default:
	}
}
