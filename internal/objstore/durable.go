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
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
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
//     it (TestS3Compat X6). The generation's MD5 comes from the caller's digest (vactor computes it in
//     its sha256 pass); only a digest without one costs an extra read of the file before the PUT. Each
//     generation records x-amz-meta-{sha256, epoch, generation, key, format} (guide I4: enough to rebuild
//     its manifest), and a HEAD after the PUT checks the size and the sha256 metadata; the server does not
//     check the sha256 itself (minio-go's AutoChecksum does not add
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

func (d *Durable) ReadManifest(ctx context.Context, key string) (durable.Manifest, durable.Version, error) {
	m, v, err := d.readManifest(ctx, key)
	if err != nil {
		return durable.Manifest{}, durable.NoVersion, err
	}
	return m, v, nil
}

// readManifest is ReadManifest that also returns the version of a manifest that does not decode
// (ErrBadManifest), so a publish conditional on that version can replace it.
func (d *Durable) readManifest(ctx context.Context, key string) (durable.Manifest, durable.Version, error) {
	name := d.root + durable.ManifestObject(key)
	var b []byte
	var etag string
	err := d.s.retryIdempotent(ctx, OpGetManifest, name, d.s.cfg.OpTimeout, func(a *attempt) error {
		body, info, _, err := d.s.core.GetObject(a.ctx, d.s.bucket, name, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		defer body.Close()
		b, err = io.ReadAll(io.LimitReader(body, 1<<16))
		etag = info.ETag
		return err
	})
	if err != nil {
		if noSuchKey(err) {
			err = errors.Join(durable.ErrNoManifest, err)
		}
		return durable.Manifest{}, durable.NoVersion, wrap(err, "cannot read manifest %s; nothing changed, safe to retry", name)
	}
	d.s.moved(OpGetManifest, int64(len(b)))
	if etag == "" {
		return durable.Manifest{}, durable.NoVersion, errs.Wrap(module, nil, "manifest %s has no ETag; conditional publish impossible", name)
	}
	m, err := durable.Decode(key, b)
	if err != nil {
		return durable.Manifest{}, durable.Version(etag), wrap(err, "manifest %s (version %s) is unreadable; repair it with dbtool rebuild-manifest", name, etag)
	}
	return m, durable.Version(etag), nil
}

func (d *Durable) PutGeneration(ctx context.Context, m durable.Manifest, path string) error {
	name, dg := d.root+m.Object, m.Digest()
	if dg.Size > maxSinglePut {
		return errs.Wrap(module, nil, "cannot upload %s: %d bytes is over the 5 GiB single-PUT limit; nothing uploaded", name, dg.Size)
	}
	f, err := os.Open(path)
	if err != nil {
		return wrap(err, "cannot upload %s; nothing uploaded, safe to retry", name)
	}
	defer f.Close()
	var sum string
	if dg.MD5 != "" {
		sum, err = contentMD5(dg.MD5)
	} else {
		sum, err = d.s.md5File(f) // the caller did not compute it in its hashing pass: one more read
	}
	if err != nil {
		return wrap(err, "cannot upload %s: no Content-MD5 for %s; nothing uploaded, safe to retry", name, path)
	}
	opts := minio.PutObjectOptions{ContentType: "application/vnd.sqlite3", UserMetadata: metadata(m.ObjectInfo())}
	opts.SetMatchETagExcept("*") // create-only
	err = d.s.sendOnce(ctx, OpPutGen, name, d.s.transferTime(dg.Size), func(a *attempt) error {
		_, err := d.s.core.PutObject(a.ctx, d.s.bucket, name, a.watch(f, dg.Size, d.s.cfg.StallTimeout), dg.Size, sum, "", opts)
		return err
	})
	if err != nil {
		if Classify(err) == Precondition {
			err = errors.Join(durable.ErrExists, err)
		}
		return wrap(err, "cannot upload %s; no manifest names it, safe to retry with a new generation", name)
	}
	d.s.moved(OpPutGen, dg.Size)
	info, err := d.stat(ctx, name)
	if err != nil {
		return wrap(err, "uploaded %s but cannot verify it; no manifest names it, safe to retry with a new generation", name)
	}
	if !info.Holds(dg) {
		return errs.Wrap(module, nil, "uploaded %s but the store holds %d bytes sha256 %q, not %d bytes %s; no manifest names it, safe to retry with a new generation",
			name, info.Size, info.SHA256, dg.Size, dg.SHA256)
	}
	return nil
}

// contentMD5 turns a hex MD5 into a Content-MD5 header value (base64).
func contentMD5(hexSum string) (string, error) {
	b, err := hex.DecodeString(hexSum)
	if err == nil && len(b) != md5.Size {
		err = errs.Wrap(module, nil, "md5 %q is not %d bytes", hexSum, md5.Size)
	}
	return base64.StdEncoding.EncodeToString(b), err
}

// StatGeneration HEADs a generation object (retried while Transient) and returns its size and its
// x-amz-meta-sha256.
func (d *Durable) StatGeneration(ctx context.Context, object string) (durable.GenInfo, error) {
	name := d.root + object
	info, err := d.stat(ctx, name)
	if err != nil {
		if noSuchKey(err) {
			err = errors.Join(durable.ErrNotFound, err)
		}
		return durable.GenInfo{}, wrap(err, "cannot stat generation %s; nothing changed, safe to retry", name)
	}
	return info, nil
}

// stat HEADs the full object name.
func (d *Durable) stat(ctx context.Context, name string) (durable.GenInfo, error) {
	var st minio.ObjectInfo
	err := d.s.retryIdempotent(ctx, OpHead, name, d.s.cfg.OpTimeout, func(a *attempt) error {
		var err error
		st, err = d.s.c.StatObject(a.ctx, d.s.bucket, name, minio.StatObjectOptions{})
		return err
	})
	return genInfo(st.Size, st.Metadata), err
}

// metadata is a generation's x-amz-meta-* (object-storage guide I4). The key is path-escaped, as in the
// object name: header values must be ASCII.
func metadata(g durable.GenInfo) map[string]string {
	return map[string]string{"sha256": g.SHA256, "epoch": strconv.FormatUint(g.Epoch, 10),
		"generation": strconv.FormatUint(g.Generation, 10), "key": url.PathEscape(g.Key), "format": g.Format}
}

// genInfo parses what metadata wrote. A missing or malformed field stays zero: the HEAD is evidence, not
// input to trust (callers compare it with what they expect).
func genInfo(size int64, h http.Header) durable.GenInfo {
	g := durable.GenInfo{Size: size, SHA256: h.Get("X-Amz-Meta-Sha256"), Format: h.Get("X-Amz-Meta-Format")}
	g.Epoch, _ = strconv.ParseUint(h.Get("X-Amz-Meta-Epoch"), 10, 64)
	g.Generation, _ = strconv.ParseUint(h.Get("X-Amz-Meta-Generation"), 10, 64)
	g.Key, _ = url.PathUnescape(h.Get("X-Amz-Meta-Key"))
	return g
}

func (d *Durable) FetchGeneration(ctx context.Context, m durable.Manifest, path string) (durable.Fetched, error) {
	name := d.root + m.Object
	t0 := time.Now()
	var st durable.Fetched
	err := d.s.retryIdempotent(ctx, OpFetch, name, d.s.cfg.OpTimeout, func(a *attempt) error {
		h := sha256.New()
		n, _, ttfb, err := d.s.getToPart(a, name, path, h)
		st.TTFB = ttfb
		if err == nil && (n != m.Size || hex.EncodeToString(h.Sum(nil)) != m.SHA256) {
			err = errs.Wrap(module, durable.ErrCorrupt, "generation %s is %d bytes sha256 %x; manifest says %d bytes %s",
				name, n, h.Sum(nil), m.Size, m.SHA256)
		}
		if err = finishPart(path, err); err == nil {
			st.Bytes = n
		}
		return err
	})
	if err != nil {
		if noSuchKey(err) {
			err = errors.Join(durable.ErrNotFound, err)
		}
		return st, wrap(err, "cannot download generation %s; nothing cached", name)
	}
	st.Total = time.Since(t0)
	d.s.moved(OpFetch, st.Bytes)
	return st, nil
}

func (d *Durable) PublishManifest(ctx context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	name := d.root + durable.ManifestObject(m.Key)
	stored, v, err := d.readManifest(ctx, m.Key)
	switch {
	case errors.Is(err, durable.ErrNoManifest):
		v = durable.NoVersion
	case errors.Is(err, durable.ErrBadManifest): // no epoch to compare: only If-Match on its version applies
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
	err = d.s.sendOnce(ctx, OpPublish, name, d.s.cfg.OpTimeout, func(a *attempt) error {
		var err error
		info, err = d.s.core.PutObject(a.ctx, d.s.bucket, name, bytes.NewReader(b), int64(len(b)),
			base64.StdEncoding.EncodeToString(sum[:]), "", opts)
		return err
	})
	if err != nil {
		if Classify(err) == Precondition {
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

var _ durable.GCStore = (*Durable)(nil)

// DeleteGenerations deletes key's generation objects in batches of up to 1000 (guide GC5), after checking
// every name is one (CheckDeletable, GC4).
func (d *Durable) DeleteGenerations(ctx context.Context, key string, names []string) error {
	if err := durable.CheckDeletable(key, names); err != nil {
		return err
	}
	full := make([]string, len(names))
	for i, n := range names {
		full[i] = d.root + n
	}
	return d.s.Delete(ctx, full)
}

// ListKey lists KeyPrefix(key) page by page (GC and dbtool only, guide K2).
func (d *Durable) ListKey(ctx context.Context, key string) (durable.Listing, error) {
	var l durable.Listing
	man, gen := durable.ManifestObject(key), durable.GenPrefix(key)
	err := d.s.EachPage(ctx, d.root+durable.KeyPrefix(key), "", func(p Page) error {
		for _, o := range p.Objects {
			name := strings.TrimPrefix(o.Key, d.root)
			so := durable.StoredObject{Name: name, Size: o.Size, Modified: o.Modified, Version: durable.Version(o.ETag)}
			switch {
			case name == man:
				l.Manifest = &so
			case strings.HasPrefix(name, gen):
				l.Generations = append(l.Generations, so)
			}
		}
		return nil
	})
	if err != nil {
		return durable.Listing{}, err
	}
	slices.SortFunc(l.Generations, func(a, b durable.StoredObject) int { return strings.Compare(a.Name, b.Name) })
	return l, nil
}

// ListKeys lists the database keys of one shard ("dbs/<shard>/<escaped key>/"), one page at a time.
// Prefixes that are not the canonical KeyPrefix of their unescaped key are not ours and are skipped.
func (d *Durable) ListKeys(ctx context.Context, shard string, fn func(key string) error) error {
	if !durable.ValidShard(shard) {
		return errs.Wrap(module, nil, "cannot list shard %q: not two lower-case hex digits", shard)
	}
	base := d.root + "dbs/" + shard + "/"
	return d.s.EachPage(ctx, base, "/", func(p Page) error {
		for _, pre := range p.Prefixes {
			key, err := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(pre, base), "/"))
			if err != nil || d.root+durable.KeyPrefix(key) != pre {
				continue
			}
			if err := fn(key); err != nil {
				return err
			}
		}
		return nil
	})
}
