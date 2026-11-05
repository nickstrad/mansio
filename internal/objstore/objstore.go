// Package objstore is the S3 adapter (minio-go) for moving SQLite files to and from object storage
// (SeaweedFS in the coldload test). Errors leaving it are errs.Error with module "objstore"; a missing object
// matches ErrNotFound.
package objstore

import (
	"context"
	"errors"
	"io"
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

// Config says where the store is. All fields but MaxConns are required.
type Config struct {
	Endpoint  string // host:port, e.g. localhost:8333
	AccessKey string
	SecretKey string
	Bucket    string
	// MaxConns caps open connections per host; set it to at least the fetch concurrency, or fetches queue
	// for a socket. Default 1024.
	MaxConns int
}

// Store is safe for concurrent use.
type Store struct {
	c      *minio.Client
	core   minio.Core
	bucket string
	bufs   chan []byte // free list of copy buffers; survives GC, unlike sync.Pool
}

const (
	copyBuf  = 1 << 20
	keptBufs = 64 // at most 64 MiB of idle buffers
)

func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, errs.Wrap(module, nil, "cannot create S3 client: endpoint, access key, secret key and bucket are all required")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 1024
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          cfg.MaxConns,
		MaxIdleConnsPerHost:   cfg.MaxConns,
		MaxConnsPerHost:       cfg.MaxConns,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true,
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Transport: tr,
	})
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot create S3 client for %s", cfg.Endpoint)
	}
	return &Store{c: c, core: minio.Core{Client: c}, bucket: cfg.Bucket, bufs: make(chan []byte, keptBufs)}, nil
}

// EnsureBucket creates the bucket if it does not exist.
func (s *Store) EnsureBucket(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return errs.Wrap(module, err, "cannot check bucket %s (is the store up?)", s.bucket)
	}
	if ok {
		return nil
	}
	if err := s.c.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
		return errs.Wrap(module, err, "cannot create bucket %s", s.bucket)
	}
	return nil
}

// PutFile uploads the local file at path as key and returns the bytes sent.
func (s *Store) PutFile(ctx context.Context, key, path string) (int64, error) {
	info, err := s.c.FPutObject(ctx, s.bucket, key, path, minio.PutObjectOptions{
		ContentType: "application/vnd.sqlite3",
		PartSize:    64 << 20,
	})
	if err != nil {
		return 0, errs.Wrap(module, err, "cannot upload %s as %s; safe to retry", path, key)
	}
	return info.Size, nil
}

// FetchStats splits a download into time to first byte and the rest.
type FetchStats struct {
	Bytes int64
	TTFB  time.Duration // GET sent → response headers
	Total time.Duration // GET sent → local file closed (not fsynced)
}

// GetToFile downloads key to path with one GET.
//
//	post: err == nil ⇒ path holds exactly the object's bytes (length checked against Content-Length)
//	post: err != nil ⇒ neither path nor path+".part" exists (unless path existed before and the GET failed)
//	post: a missing object matches errors.Is(err, ErrNotFound)
//
// The file is not fsynced: it is a local cache.
func (s *Store) GetToFile(ctx context.Context, key, path string) (FetchStats, error) {
	t0 := time.Now()
	body, info, _, err := s.core.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			err = errors.Join(ErrNotFound, err)
		}
		return FetchStats{}, errs.Wrap(module, err, "cannot download %s", key).With("bucket", s.bucket)
	}
	defer body.Close()
	st := FetchStats{TTFB: time.Since(t0)}

	n, err := s.writeFile(path+".part", body)
	if err == nil && info.Size >= 0 && n != info.Size {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = os.Rename(path+".part", path)
	}
	if err != nil {
		os.Remove(path + ".part")
		return st, errs.Wrap(module, err, "cannot download %s to %s (got %d of %d bytes); nothing kept, safe to retry",
			key, path, n, info.Size)
	}
	st.Bytes, st.Total = n, time.Since(t0)
	return st, nil
}

// writeFile copies r into a new file at path through a pooled 1 MiB buffer. A plain read/write loop on
// purpose: io.CopyBuffer would hand the copy to (*os.File).ReadFrom, which ignores the buffer.
func (s *Store) writeFile(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	buf := s.getBuf()
	defer s.putBuf(buf)
	var n int64
	for {
		m, rerr := io.ReadFull(r, buf)
		if m > 0 {
			if _, werr := f.Write(buf[:m]); werr != nil {
				f.Close()
				return n, werr
			}
			n += int64(m)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			f.Close()
			return n, rerr
		}
	}
	return n, f.Close()
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

// List returns key → size for every object under prefix.
func (s *Store) List(ctx context.Context, prefix string) (map[string]int64, error) {
	out := make(map[string]int64)
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, errs.Wrap(module, o.Err, "cannot list %s/%s (is the store up?)", s.bucket, prefix)
		}
		out[o.Key] = o.Size
	}
	return out, nil
}

// DeletePrefix removes every object under prefix and returns how many it removed.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	have, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	objs := make(chan minio.ObjectInfo, len(have)) // buffered to the count: the feeder never blocks
	for k := range have {
		objs <- minio.ObjectInfo{Key: k}
	}
	close(objs)
	for e := range s.c.RemoveObjects(ctx, s.bucket, objs, minio.RemoveObjectsOptions{}) { // reports failures only
		if e.Err != nil {
			return 0, errs.Wrap(module, e.Err, "cannot delete %s; some objects under %s may remain, safe to retry", e.ObjectName, prefix)
		}
	}
	left, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	if len(left) > 0 {
		return len(have) - len(left), errs.Wrap(module, nil, "%d objects under %s were not deleted; safe to retry", len(left), prefix)
	}
	return len(have), nil
}
