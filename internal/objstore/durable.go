package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
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
// What is atomic, and what is verified (2026-11-05):
//
//   - Generation uploads are one PUT with `If-None-Match: *` (create-only); a 412 is durable.ErrExists. One
//     PUT is all-or-nothing on S3: a failed upload leaves no object. Files over 5 GiB are refused (a
//     multipart upload would need the condition on CompleteMultipartUpload).
//   - The manifest publish is GET (version = ETag, epoch check), then PUT with `If-Match: <etag>` (or
//     `If-None-Match: *` when none exists); a 412 is durable.ErrStale. The conditional PUT is what makes
//     "compare version, then write" atomic. AWS S3 documents both headers on PutObject (2024); SeaweedFS's
//     wiki page "S3 Conditional Operations" documents both and says "the conditional check and the write
//     are atomic — cluster-wide" (version not stated). UNVERIFIED here: Docker was down, so the contract
//     suite (which includes a concurrent-publish test) has not run against SeaweedFS. A server that
//     ignores the headers would make publishes last-writer-wins: run TestS3DurableStoreContract first.
//   - Upload integrity: the size is verified with a HEAD after the PUT, and the sha256 is recorded as
//     x-amz-meta-sha256 (the manifest holds it too). The server does not check the hash: minio-go's
//     AutoChecksum does not add x-amz-checksum-sha256 to this single streaming-signed PUT (seen in
//     TestDurableStoreContractAgainstFakeS3), and Checksum would need TrailingHeaders on the client. The
//     V4 streaming signature covers each chunk, so the transport is protected; FetchGeneration re-hashes
//     every download and fails with durable.ErrCorrupt on a mismatch, so a bad object is never activated.
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
	body, info, _, err := d.s.core.GetObject(ctx, d.s.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			err = errors.Join(durable.ErrNoManifest, err)
		}
		return durable.Manifest{}, durable.NoVersion, errs.Wrap(module, err, "cannot read manifest %s; nothing changed, safe to retry", name)
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, 1<<16))
	if err != nil {
		return durable.Manifest{}, durable.NoVersion, errs.Wrap(module, err, "cannot read manifest %s; nothing changed, safe to retry", name)
	}
	m, err := durable.Decode(key, b)
	if err != nil {
		return durable.Manifest{}, durable.NoVersion, errs.Wrap(module, err, "manifest %s is unreadable", name)
	}
	if info.ETag == "" {
		return durable.Manifest{}, durable.NoVersion, errs.Wrap(module, nil, "manifest %s has no ETag; conditional publish impossible", name)
	}
	return m, durable.Version(info.ETag), nil
}

func (d *Durable) PutGeneration(ctx context.Context, key string, gen uint64, path string, dg durable.Digest) error {
	name := d.root + durable.GenObject(key, gen)
	if dg.Size > maxSinglePut {
		return errs.Wrap(module, nil, "cannot upload %s: %d bytes is over the 5 GiB single-PUT limit; nothing uploaded", name, dg.Size)
	}
	f, err := os.Open(path)
	if err != nil {
		return errs.Wrap(module, err, "cannot upload %s; nothing uploaded, safe to retry", name)
	}
	defer f.Close()
	opts := minio.PutObjectOptions{ContentType: "application/vnd.sqlite3", DisableMultipart: true,
		UserMetadata: map[string]string{"sha256": dg.SHA256}}
	opts.SetMatchETagExcept("*") // create-only
	if _, err := d.s.c.PutObject(ctx, d.s.bucket, name, f, dg.Size, opts); err != nil {
		if preconditionFailed(err) {
			err = errors.Join(durable.ErrExists, err)
		}
		return errs.Wrap(module, err, "cannot upload %s; no manifest names it, safe to retry with a new generation", name)
	}
	st, err := d.s.c.StatObject(ctx, d.s.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		return errs.Wrap(module, err, "uploaded %s but cannot verify it; no manifest names it, safe to retry with a new generation", name)
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
	body, _, _, err := d.s.core.GetObject(ctx, d.s.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			err = errors.Join(durable.ErrNotFound, err)
		}
		return durable.Fetched{}, errs.Wrap(module, err, "cannot download generation %s; nothing cached, safe to retry", name)
	}
	defer body.Close()
	st := durable.Fetched{TTFB: time.Since(t0)}
	h := sha256.New()
	n, err := d.s.writeFile(path+".part", body, h)
	if err == nil && (n != m.Size || hex.EncodeToString(h.Sum(nil)) != m.SHA256) {
		err = errs.Wrap(module, durable.ErrCorrupt, "generation %s is %d bytes sha256 %x; manifest says %d bytes %s",
			name, n, h.Sum(nil), m.Size, m.SHA256)
	}
	if err == nil {
		err = os.Rename(path+".part", path)
	}
	if err != nil {
		os.Remove(path + ".part")
		return st, errs.Wrap(module, err, "cannot download generation %s; nothing cached", name)
	}
	st.Bytes, st.Total = n, time.Since(t0)
	return st, nil
}

func (d *Durable) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	name := d.root + durable.ManifestObject(m.Key)
	stored, v, err := d.ReadManifest(ctx, m.Key)
	switch {
	case errors.Is(err, durable.ErrNoManifest):
		v = durable.NoVersion
	case err != nil:
		return durable.NoVersion, errs.Wrap(module, err, "cannot publish %s; nothing changed, safe to retry", name)
	case stored.Epoch > m.Epoch:
		return durable.NoVersion, errs.Wrap(module, durable.ErrStale, "cannot publish %s at epoch %d: stored epoch is %d; nothing changed", name, m.Epoch, stored.Epoch)
	}
	if v != prev {
		return durable.NoVersion, errs.Wrap(module, durable.ErrStale, "cannot publish %s: version is %q, caller read %q; nothing changed", name, v, prev)
	}
	b := durable.Encode(m)
	opts := minio.PutObjectOptions{ContentType: "application/json", DisableMultipart: true}
	if prev == durable.NoVersion {
		opts.SetMatchETagExcept("*")
	} else {
		opts.SetMatchETag(string(prev))
	}
	info, err := d.s.c.PutObject(ctx, d.s.bucket, name, bytes.NewReader(b), int64(len(b)), opts)
	if err != nil {
		if preconditionFailed(err) {
			return durable.NoVersion, errs.Wrap(module, errors.Join(durable.ErrStale, err), "cannot publish %s: it changed meanwhile; nothing changed", name)
		}
		return durable.NoVersion, errs.Wrap(module, err, "publish of %s failed; it may or may not have applied, re-read before retrying", name)
	}
	if info.ETag == "" {
		return durable.NoVersion, errs.Wrap(module, nil, "published %s but got no ETag; re-read before retrying", name)
	}
	return durable.Version(info.ETag), nil
}
