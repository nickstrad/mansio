package objstore

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/errs"
)

// Durable is the S3 adapter for durable.Store (contract 1). Object names are Root + the durable layout.
//
// What is atomic, and what is verified:
//
//   - Generation uploads are one PUT with `If-None-Match: *` (create-only); a 412 is durable.ErrExists. One
//     PUT is all-or-nothing on S3: a failed upload leaves no object. Files over 5 GiB are refused (a
//     multipart upload would need the condition on CompleteMultipartUpload).
//   - The manifest publish is GET (version = ETag, epoch check), then PUT with `If-Match: <etag>` (or
//     `If-None-Match: *` when none exists); a 412 is durable.ErrStale. The conditional PUT is what makes
//     "compare version, then write" atomic. AWS S3 documents both headers on PutObject (2024). Verified on
//     single-node SeaweedFS 4.47 (2026-11-05): TestS3DurableStoreContract, including concurrent publishes on
//     one version (exactly one wins), see knowledge/writeback/2026-11-05-s3-conditional-put-verified.md.
//     Any other backend or version must pass TestS3Compat first (object-storage guide §7): a server that
//     ignores the headers would make publishes last-writer-wins.
//   - Retries (object-storage guide R1–R3): the GETs and the HEAD are retried while Transient; the PUTs are
//     sent exactly once. A PUT that fails without an answer is ambiguous and says so; the caller re-reads.
//     minio-go's own retries are off, so a lost reply is never re-sent and misread as a 412.
//   - Upload integrity (object-storage guide V1, V2): every PUT carries Content-MD5, so the server rejects a
//     body that differs from what we read (BadDigest, Permanent) and stores nothing; SeaweedFS 4.47 enforces
//     it (TestS3Compat X6). The generation's MD5 costs one extra read of the local file before the PUT. The
//     size is also verified with a HEAD after the PUT, and the sha256 is recorded as x-amz-meta-sha256 (the
//     manifest holds it too); the server does not check that one (minio-go's AutoChecksum does not add
//     x-amz-checksum-sha256 to a single streaming-signed PUT). FetchGeneration re-hashes every download and
//     fails with durable.ErrCorrupt on a mismatch, so a bad object is never activated.
type Durable struct {
	s    *Store
	root string
}

// NewDurable returns the durable store over s. root prefixes every object name ("" for none; tests use a
// random one).
func NewDurable(s *Store, root string) *Durable { return &Durable{s: s, root: root} }

const maxSinglePut = 5 << 30

func preconditionFailed(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == minio.PreconditionFailed || code == "ConditionalRequestConflict"
}

func (d *Durable) ReadManifest(ctx context.Context, key string) (durable.Manifest, durable.Version, error) {
	name := d.root + durable.ManifestObject(key)
	var b []byte
	var etag string
	err := d.s.retryIdempotent(ctx, OpGetManifest, name, func(ctx context.Context) error {
		a := d.s.begin(ctx, d.s.cfg.OpTimeout)
		body, info, _, err := d.s.core.GetObject(a.ctx, d.s.bucket, name, minio.GetObjectOptions{})
		if err != nil {
			return a.end(err)
		}
		defer body.Close()
		b, err = io.ReadAll(io.LimitReader(body, 1<<16))
		etag = info.ETag
		return a.end(err)
	})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			err = errors.Join(durable.ErrNoManifest, err)
		}
		return durable.Manifest{}, durable.NoVersion, wrap(err, "cannot read manifest %s; nothing changed, safe to retry", name)
	}
	d.s.moved(OpGetManifest, int64(len(b)))
	m, err := durable.Decode(key, b)
	if err != nil {
		return durable.Manifest{}, durable.NoVersion, wrap(err, "manifest %s is unreadable", name)
	}
	if etag == "" {
		return durable.Manifest{}, durable.NoVersion, errs.Wrap(module, nil, "manifest %s has no ETag; conditional publish impossible", name)
	}
	return m, durable.Version(etag), nil
}

func (d *Durable) PutGeneration(ctx context.Context, key string, gen uint64, path string, dg durable.Digest) error {
	name := d.root + durable.GenObject(key, gen)
	if dg.Size > maxSinglePut {
		return errs.Wrap(module, nil, "cannot upload %s: %d bytes is over the 5 GiB single-PUT limit; nothing uploaded", name, dg.Size)
	}
	f, err := os.Open(path)
	if err != nil {
		return wrap(err, "cannot upload %s; nothing uploaded, safe to retry", name)
	}
	defer f.Close()
	sum, err := d.s.md5File(f)
	if err != nil {
		return wrap(err, "cannot upload %s: reading %s failed; nothing uploaded, safe to retry", name, path)
	}
	opts := minio.PutObjectOptions{ContentType: "application/vnd.sqlite3",
		UserMetadata: map[string]string{"sha256": dg.SHA256}}
	opts.SetMatchETagExcept("*") // create-only
	err = d.s.sendOnce(ctx, OpPutGen, name, func(ctx context.Context) error {
		a := d.s.begin(ctx, d.s.transferTime(dg.Size))
		_, err := d.s.core.PutObject(a.ctx, d.s.bucket, name, a.watch(f, dg.Size, d.s.cfg.StallTimeout), dg.Size, sum, "", opts)
		return a.end(err)
	})
	if err != nil {
		if preconditionFailed(err) {
			err = errors.Join(durable.ErrExists, err)
		}
		return wrap(err, "cannot upload %s; no manifest names it, safe to retry with a new generation", name)
	}
	d.s.moved(OpPutGen, dg.Size)
	var st minio.ObjectInfo
	err = d.s.retryIdempotent(ctx, OpHead, name, func(ctx context.Context) error {
		a := d.s.begin(ctx, d.s.cfg.OpTimeout)
		var err error
		st, err = d.s.c.StatObject(a.ctx, d.s.bucket, name, minio.StatObjectOptions{})
		return a.end(err)
	})
	if err != nil {
		return wrap(err, "uploaded %s but cannot verify it; no manifest names it, safe to retry with a new generation", name)
	}
	if st.Size != dg.Size {
		return errs.Wrap(module, nil, "uploaded %s but the store holds %d bytes, not %d; no manifest names it, safe to retry with a new generation",
			name, st.Size, dg.Size)
	}
	return nil
}

func (d *Durable) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (durable.Fetched, error) {
	name := d.root + m.Object
	t0 := time.Now()
	var st durable.Fetched
	err := d.s.retryIdempotent(ctx, OpFetch, name, func(ctx context.Context) error {
		var err error
		st, err = d.fetch(ctx, name, m, path)
		return err
	})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			err = errors.Join(durable.ErrNotFound, err)
		}
		return st, wrap(err, "cannot download generation %s; nothing cached", name)
	}
	st.Total = time.Since(t0)
	d.s.moved(OpFetch, st.Bytes)
	return st, nil
}

// fetch is one attempt of FetchGeneration: one GET, bounded by the size-derived deadline and the stall
// timeout, hashed while it is written to .part.
func (d *Durable) fetch(ctx context.Context, name string, m durable.Manifest, path string) (st durable.Fetched, err error) {
	t0 := time.Now()
	a := d.s.begin(ctx, d.s.transferTime(m.Size))
	defer func() { err = a.end(err) }()
	body, _, _, err := d.s.core.GetObject(a.ctx, d.s.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return st, err
	}
	defer body.Close()
	st.TTFB = time.Since(t0)
	h := sha256.New()
	n, err := d.s.writeFile(path+".part", a.watch(body, -1, d.s.cfg.StallTimeout), h)
	if err == nil && (n != m.Size || hex.EncodeToString(h.Sum(nil)) != m.SHA256) {
		err = errs.Wrap(module, durable.ErrCorrupt, "generation %s is %d bytes sha256 %x; manifest says %d bytes %s",
			name, n, h.Sum(nil), m.Size, m.SHA256)
	}
	if err == nil {
		err = os.Rename(path+".part", path)
	}
	if err != nil {
		os.Remove(path + ".part")
		return st, err
	}
	st.Bytes = n
	return st, nil
}

func (d *Durable) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	name := d.root + durable.ManifestObject(m.Key)
	stored, v, err := d.ReadManifest(ctx, m.Key)
	switch {
	case errors.Is(err, durable.ErrNoManifest):
		v = durable.NoVersion
	case err != nil:
		return durable.NoVersion, wrap(err, "cannot publish %s; nothing changed, safe to retry", name)
	case stored.Epoch > m.Epoch:
		return durable.NoVersion, wrap(durable.ErrStale, "cannot publish %s at epoch %d: stored epoch is %d; nothing changed", name, m.Epoch, stored.Epoch)
	}
	if v != prev {
		return durable.NoVersion, wrap(durable.ErrStale, "cannot publish %s: version is %q, caller read %q; nothing changed", name, v, prev)
	}
	b := durable.Encode(m)
	sum := md5.Sum(b)
	opts := minio.PutObjectOptions{ContentType: "application/json"}
	if prev == durable.NoVersion {
		opts.SetMatchETagExcept("*")
	} else {
		opts.SetMatchETag(string(prev))
	}
	var info minio.UploadInfo
	err = d.s.sendOnce(ctx, OpPublish, name, func(ctx context.Context) error {
		a := d.s.begin(ctx, d.s.cfg.OpTimeout)
		var err error
		info, err = d.s.core.PutObject(a.ctx, d.s.bucket, name, bytes.NewReader(b), int64(len(b)),
			base64.StdEncoding.EncodeToString(sum[:]), "", opts)
		return a.end(err)
	})
	if err != nil {
		if preconditionFailed(err) {
			return durable.NoVersion, wrap(errors.Join(durable.ErrStale, err), "cannot publish %s: it changed meanwhile; nothing changed", name)
		}
		return durable.NoVersion, wrap(err, "publish of %s failed; it may or may not have applied, re-read before retrying", name)
	}
	d.s.moved(OpPublish, int64(len(b)))
	if info.ETag == "" {
		return durable.NoVersion, errs.Wrap(module, nil, "published %s but got no ETag; re-read before retrying", name)
	}
	return durable.Version(info.ETag), nil
}
