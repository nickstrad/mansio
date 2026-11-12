package objstore_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/objstore"
)

// TestS3Compat records, for one S3 backend and version, every S3 behavior mansio-server relies on. Run it before
// using a backend or version, and record the table it prints in docs/notes/writeback/<date>-s3-compat-<backend>.md. A
// failure in X1–X5 fails the test (it blocks the backend); X6–X11 are recorded as pass / fail / skip and fail
// nothing.
//
//	COLDLOAD_S3=localhost:8333 S3_BACKEND=seaweedfs-4.47 go test -count=1 -v -run TestS3Compat ./internal/objstore
//
// Env: COLDLOAD_S3 (endpoint, required), S3_BACKEND (label), S3_ACCESS_KEY / S3_SECRET_KEY / S3_BUCKET (default:
// the coldload identity), S3_COMPAT_ROUNDS (X3 rounds, default 200), S3_AGENT_ACCESS_KEY / S3_AGENT_SECRET_KEY
// (the scoped agent identity for X12; default the dev keys of deploy/seaweedfs/s3.json, "-" skips X12; a
// backend that does not know the identity records a skip; X12 blocks when the identity exists), S3_COMPAT_PEER
// (a second gateway of the same cluster, e.g. localhost:8344 for deploy/seaweedfs-cluster: X3's odd writers and
// its read-back go through it, so the conditional PUT races across gateways).
func TestS3Compat(t *testing.T) {
	ep := s3Endpoint(t)
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	bucket := env("S3_BUCKET", "coldload")
	c := rawClient(t, ep, env("S3_ACCESS_KEY", "coldload"), env("S3_SECRET_KEY", "coldload-secret"))
	ctx := context.Background()
	if ok, err := c.BucketExists(ctx, bucket); err != nil || !ok {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("bucket %s: %v", bucket, err)
		}
	}
	root := "compat-" + logx.NewID() + "/"
	t.Cleanup(func() {
		objs := c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: root, Recursive: true})
		for e := range c.RemoveObjects(ctx, bucket, objs, minio.RemoveObjectsOptions{}) {
			t.Logf("cleanup: %v", e.Err)
		}
	})
	rounds := 200
	fmt.Sscan(env("S3_COMPAT_ROUNDS", "200"), &rounds)

	type result struct{ id, status, detail string }
	var results []result
	record := func(t *testing.T, id string, blocking bool, err error, detail string) {
		t.Helper()
		status := "pass"
		if err != nil {
			status, detail = "fail", err.Error()
			if blocking {
				t.Errorf("%s: %v", id, err)
			}
		}
		results = append(results, result{id, status, detail})
	}
	peerEP := os.Getenv("S3_COMPAT_PEER")
	peer := c // X3's second gateway; the same one without S3_COMPAT_PEER
	if peerEP != "" {
		peer = rawClient(t, peerEP, env("S3_ACCESS_KEY", "coldload"), env("S3_SECRET_KEY", "coldload-secret"))
	}
	putVia := func(c *minio.Client, name string, data []byte, set func(*minio.PutObjectOptions)) (minio.UploadInfo, error) {
		opts := minio.PutObjectOptions{DisableMultipart: true}
		if set != nil {
			set(&opts)
		}
		return c.PutObject(ctx, bucket, root+name, bytes.NewReader(data), int64(len(data)), opts)
	}
	put := func(name string, data []byte, set func(*minio.PutObjectOptions)) (minio.UploadInfo, error) {
		return putVia(c, name, data, set)
	}
	status := func(err error) int { return minio.ToErrorResponse(err).StatusCode }

	t.Run("X1 If-None-Match create-only", func(t *testing.T) {
		_, err := put("x1", []byte("a"), func(o *minio.PutObjectOptions) { o.SetMatchETagExcept("*") })
		if err == nil {
			_, err = put("x1", []byte("b"), func(o *minio.PutObjectOptions) { o.SetMatchETagExcept("*") })
			if status(err) == 412 {
				err = nil
			} else {
				err = fmt.Errorf("second create: %v (status %d), want 412", err, status(err))
			}
		}
		record(t, "X1", true, err, "second create → 412")
	})

	t.Run("X2 If-Match on a stale ETag", func(t *testing.T) {
		first, err := put("x2", []byte("a"), nil)
		if err == nil {
			_, err = put("x2", []byte("b"), func(o *minio.PutObjectOptions) { o.SetMatchETag(first.ETag) })
		}
		if err == nil {
			_, err = put("x2", []byte("c"), func(o *minio.PutObjectOptions) { o.SetMatchETag(first.ETag) })
			if s := status(err); s == 412 || s == 409 {
				err = nil
			} else {
				err = fmt.Errorf("PUT on stale ETag: %v (status %d), want 412 or 409", err, s)
			}
		}
		record(t, "X2", true, err, "current ETag → 200, stale ETag → 412")
	})

	t.Run("X3 conditional PUT is atomic under concurrency", func(t *testing.T) {
		const writers = 8
		info, err := put("x3", []byte("base"), nil)
		etag := info.ETag
		var bad error
		peerWins := 0
		for r := 0; r < rounds && err == nil && bad == nil; r++ {
			errs := make([]error, writers)
			infos := make([]minio.UploadInfo, writers)
			var wg sync.WaitGroup
			for w := range writers {
				wg.Go(func() {
					via := c
					if w%2 == 1 {
						via = peer
					}
					infos[w], errs[w] = putVia(via, "x3", fmt.Appendf(nil, "round %d writer %d", r, w),
						func(o *minio.PutObjectOptions) { o.SetMatchETag(etag) })
				})
			}
			wg.Wait()
			won := -1
			for w, e := range errs {
				switch {
				case e == nil && won >= 0:
					bad = fmt.Errorf("round %d: writers %d and %d both won", r, won, w)
				case e == nil:
					won = w
				case status(e) != 412 && status(e) != 409:
					bad = fmt.Errorf("round %d writer %d: %v", r, w, e)
				}
			}
			if bad == nil && won < 0 {
				bad = fmt.Errorf("round %d: no writer won", r)
			}
			if bad == nil {
				got, gerr := peer.GetObject(ctx, bucket, root+"x3", minio.GetObjectOptions{})
				var b bytes.Buffer
				if gerr == nil {
					_, gerr = b.ReadFrom(got)
				}
				if want := fmt.Sprintf("round %d writer %d", r, won); gerr != nil || b.String() != want {
					bad = fmt.Errorf("round %d: stored %q (%v), winner wrote %q", r, b.String(), gerr, want)
				}
				etag = infos[won].ETag
				peerWins += won % 2
			}
		}
		detail := fmt.Sprintf("%d rounds × %d If-Match writers: exactly 1 winner each, stored bytes = winner's", rounds, writers)
		if peerEP != "" {
			detail += fmt.Sprintf(" (odd writers and the read-back via %s; it won %d rounds)", peerEP, peerWins)
		}
		record(t, "X3", true, errors.Join(err, bad), detail)
	})

	t.Run("X4 strong read-after-write", func(t *testing.T) {
		var err error
		for i := range 50 {
			data := fmt.Appendf(nil, "version %d", i)
			var info minio.UploadInfo
			if info, err = put("x4", data, nil); err != nil {
				break
			}
			st, serr := c.StatObject(ctx, bucket, root+"x4", minio.StatObjectOptions{})
			o, gerr := c.GetObject(ctx, bucket, root+"x4", minio.GetObjectOptions{})
			var b bytes.Buffer
			if gerr == nil {
				_, gerr = b.ReadFrom(o)
			}
			if serr != nil || gerr != nil || st.ETag != info.ETag || !bytes.Equal(b.Bytes(), data) {
				err = fmt.Errorf("overwrite %d: HEAD etag %q (%v), GET %q (%v); PUT etag %q", i, st.ETag, serr, b.String(), gerr, info.ETag)
				break
			}
		}
		record(t, "X4", true, err, "50 overwrites: HEAD and GET right after each PUT see its ETag and bytes")
	})

	t.Run("X5 ETag changes on every overwrite", func(t *testing.T) {
		seen := map[string]bool{}
		var err error
		for i := range 20 {
			info, perr := put("x5", fmt.Appendf(nil, "distinct %d", i), nil)
			if perr != nil || seen[info.ETag] {
				err = fmt.Errorf("overwrite %d: etag %q repeated or %v", i, info.ETag, perr)
				break
			}
			seen[info.ETag] = true
		}
		same := "unknown"
		if a, e1 := put("x5", []byte("same"), nil); e1 == nil {
			if b, e2 := put("x5", []byte("same"), nil); e2 == nil {
				same = fmt.Sprintf("identical bytes repeat the ETag: %v (%s)", a.ETag == b.ETag, a.ETag)
			}
		}
		record(t, "X5", true, err, "20 distinct overwrites → 20 ETags; "+same)
	})

	t.Run("X6 Content-MD5 mismatch rejected", func(t *testing.T) {
		data := []byte("the body")
		wrong := md5.Sum([]byte("another body"))
		core := minio.Core{Client: c}
		_, err := core.PutObject(ctx, bucket, root+"x6", bytes.NewReader(data), int64(len(data)),
			base64.StdEncoding.EncodeToString(wrong[:]), "", minio.PutObjectOptions{})
		code := minio.ToErrorResponse(err).Code
		_, serr := c.StatObject(ctx, bucket, root+"x6", minio.StatObjectOptions{})
		stored := serr == nil
		var res error
		if err == nil || stored {
			res = fmt.Errorf("mismatched Content-MD5 accepted: err %v, object stored %v", err, stored)
		}
		right := md5.Sum(data)
		_, okErr := core.PutObject(ctx, bucket, root+"x6ok", bytes.NewReader(data), int64(len(data)),
			base64.StdEncoding.EncodeToString(right[:]), "", minio.PutObjectOptions{})
		if okErr != nil {
			res = errors.Join(res, fmt.Errorf("matching Content-MD5 rejected: %v", okErr))
		}
		record(t, "X6", false, res, fmt.Sprintf("wrong MD5 → %s (%s), nothing stored; right MD5 → 200", code, objstore.Classify(err)))
	})

	t.Run("X7 user metadata round-trips", func(t *testing.T) {
		meta := map[string]string{"sha256": strings.Repeat("ab", 32), "epoch": "7", "generation": "12", "key": "t1/db"}
		_, err := put("x7", []byte("m"), func(o *minio.PutObjectOptions) { o.UserMetadata = meta })
		if err == nil {
			st, serr := c.StatObject(ctx, bucket, root+"x7", minio.StatObjectOptions{})
			o, gerr := c.GetObject(ctx, bucket, root+"x7", minio.GetObjectOptions{})
			var gst minio.ObjectInfo
			if gerr == nil {
				gst, gerr = o.Stat()
			}
			err = errors.Join(serr, gerr)
			for k, v := range meta {
				if err == nil && (st.UserMetadata[k] != v && st.Metadata.Get("X-Amz-Meta-"+k) != v ||
					gst.Metadata.Get("X-Amz-Meta-"+k) != v) {
					err = fmt.Errorf("%s: HEAD %q, GET %q, want %q", k, st.Metadata.Get("X-Amz-Meta-"+k), gst.Metadata.Get("X-Amz-Meta-"+k), v)
				}
			}
		}
		record(t, "X7", false, err, "sha256, epoch, generation, key on HEAD and GET")
	})

	t.Run("X8 412 is distinguishable from 5xx", func(t *testing.T) {
		_, err := put("x8", []byte("a"), nil)
		if err == nil {
			_, err = put("x8", []byte("b"), func(o *minio.PutObjectOptions) { o.SetMatchETagExcept("*") })
			r := minio.ToErrorResponse(err)
			if r.Code == minio.PreconditionFailed && r.StatusCode == 412 && objstore.Classify(err) == objstore.Precondition {
				err = nil
			} else {
				err = fmt.Errorf("got code %q status %d kind %v", r.Code, r.StatusCode, objstore.Classify(err))
			}
		}
		record(t, "X8", false, err, "ToErrorResponse: Code PreconditionFailed, status 412, Classify Precondition")
	})

	t.Run("X9 LIST after PUT", func(t *testing.T) {
		var keys []string
		var err error
		for _, k := range []string{"x9/b", "x9/a", "x9/c/1", "x9/c/0", "x9/d"} {
			if _, err = put(k, []byte(k), nil); err != nil {
				break
			}
			keys = append(keys, root+k)
		}
		var got []string
		if err == nil {
			for o := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: root + "x9/", Recursive: true, MaxKeys: 2}) {
				if o.Err != nil {
					err = o.Err
					break
				}
				got = append(got, o.Key)
			}
		}
		slices.Sort(keys)
		if err == nil && !slices.Equal(got, keys) {
			err = fmt.Errorf("listed %v, want %v (lexicographic, 2 per page)", got, keys)
		}
		record(t, "X9", false, err, "5 keys right after PUT, pages of 2, lexicographic order")
	})

	t.Run("X10 batch delete", func(t *testing.T) {
		var err error
		for _, k := range []string{"x10/a", "x10/b"} {
			if _, err = put(k, []byte(k), nil); err != nil {
				break
			}
		}
		if err == nil {
			objs := make(chan minio.ObjectInfo, 3)
			for _, k := range []string{"x10/a", "x10/b", "x10/missing"} {
				objs <- minio.ObjectInfo{Key: root + k}
			}
			close(objs)
			for e := range c.RemoveObjects(ctx, bucket, objs, minio.RemoveObjectsOptions{}) {
				err = errors.Join(err, fmt.Errorf("%s: %w", e.ObjectName, e.Err))
			}
		}
		if err == nil {
			for o := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: root + "x10/", Recursive: true}) {
				err = errors.Join(err, fmt.Errorf("left after delete: %s %v", o.Key, o.Err))
			}
		}
		record(t, "X10", false, err, "RemoveObjects of 2 keys + 1 missing: no errors, prefix empty")
	})

	t.Run("X11 If-None-Match on CompleteMultipartUpload", func(t *testing.T) {
		core := minio.Core{Client: c}
		complete := func(data []byte) error {
			id, err := core.NewMultipartUpload(ctx, bucket, root+"x11", minio.PutObjectOptions{})
			if err != nil {
				return err
			}
			p, err := core.PutObjectPart(ctx, bucket, root+"x11", id, 1, bytes.NewReader(data), int64(len(data)), minio.PutObjectPartOptions{})
			if err != nil {
				core.AbortMultipartUpload(ctx, bucket, root+"x11", id)
				return err
			}
			opts := minio.PutObjectOptions{}
			opts.SetMatchETagExcept("*")
			_, err = core.CompleteMultipartUpload(ctx, bucket, root+"x11", id, []minio.CompletePart{{PartNumber: 1, ETag: p.ETag}}, opts)
			if err != nil {
				core.AbortMultipartUpload(ctx, bucket, root+"x11", id)
			}
			return err
		}
		err := complete([]byte("first"))
		if err == nil {
			err2 := complete([]byte("second"))
			if s := status(err2); s != 412 {
				err = fmt.Errorf("second create-only completion: %v (status %d), want 412", err2, s)
			}
		}
		if err == nil {
			o, gerr := c.GetObject(ctx, bucket, root+"x11", minio.GetObjectOptions{})
			var b bytes.Buffer
			if gerr == nil {
				_, gerr = b.ReadFrom(o)
			}
			if gerr != nil || b.String() != "first" {
				err = fmt.Errorf("object holds %q (%v), want the first upload", b.String(), gerr)
			}
		}
		record(t, "X11", false, err, "second create-only multipart completion → 412, first upload kept")
	})

	t.Run("X12 scoped identity", func(t *testing.T) {
		// The agent identity (deploy/seaweedfs/s3.json) may read, write and delete objects in the bucket, and nothing
		// else: no LIST (an agent never lists on a request path), no bucket creation, no other bucket. When the
		// identity exists the checks block; when the backend does not know it, X12 is skipped.
		ak, sk := env("S3_AGENT_ACCESS_KEY", "agent"), env("S3_AGENT_SECRET_KEY", "agent-secret-dev")
		if ak == "-" {
			results = append(results, result{"X12", "skip", "S3_AGENT_ACCESS_KEY=-"})
			return
		}
		st, err := objstore.New(objstore.Config{Endpoint: ep, AccessKey: ak, SecretKey: sk, Bucket: bucket})
		if err != nil {
			t.Fatal(err)
		}
		local := filepath.Join(t.TempDir(), "x12")
		os.WriteFile(local, []byte("x12"), 0o644)
		name := root + "x12/obj"
		var resp minio.ErrorResponse
		if _, err := st.PutFile(ctx, name, local); errors.As(err, &resp) && resp.Code == "InvalidAccessKeyId" {
			results = append(results, result{"X12", "skip", "the backend has no identity " + ak})
			return
		}
		var problems []error
		check := func(what string, err error) {
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", what, err))
			}
		}
		denied := func(what string, err error) {
			if err == nil {
				problems = append(problems, fmt.Errorf("%s: allowed, want AccessDenied", what))
			} else if k := objstore.Classify(err); k != objstore.Permanent || !strings.Contains(err.Error(), "Access Denied") &&
				!strings.Contains(err.Error(), "AccessDenied") {
				problems = append(problems, fmt.Errorf("%s: %v (%v), want AccessDenied (permanent)", what, err, k))
			}
		}
		check("bucket check (HEAD bucket)", st.EnsureBucket(ctx))
		_, err = st.PutFile(ctx, name, local)
		check("PUT", err)
		_, err = st.Size(ctx, name)
		check("HEAD", err)
		_, err = st.GetToFile(ctx, name, local+".got")
		check("GET", err)
		check("DELETE (batch)", st.Delete(ctx, []string{name}))
		if _, err := st.Size(ctx, name); !errors.Is(err, objstore.ErrNotFound) {
			problems = append(problems, fmt.Errorf("after DELETE: %v, want not found", err))
		}
		_, err = st.ListPage(ctx, root, "", "", 10)
		denied("LIST", err)
		agent := rawClient(t, ep, ak, sk)
		other := "compat-x12-" + strings.ToLower(logx.NewID())
		denied("create bucket", agent.MakeBucket(ctx, other, minio.MakeBucketOptions{}))
		if err := c.MakeBucket(ctx, other, minio.MakeBucketOptions{}); err == nil { // the admin identity makes one
			_, perr := agent.PutObject(ctx, other, "x", bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{})
			denied("PUT into another bucket", perr)
			c.RemoveObject(ctx, other, "x", minio.RemoveObjectOptions{})
			c.RemoveBucket(ctx, other)
		}
		err = errors.Join(problems...)
		record(t, "X12", true, err, "agent identity: HEAD bucket, PUT, HEAD, GET, batch DELETE allowed; LIST, create bucket, other bucket → AccessDenied (permanent)")
	})

	var b strings.Builder
	fmt.Fprintf(&b, "S3 compat results for %s at %s (%s, %d X3 rounds):\n\n| # | Result | Detail |\n|---|---|---|\n",
		env("S3_BACKEND", "unlabelled"), ep, time.Now().UTC().Format(time.DateOnly), rounds)
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.id, r.status, strings.ReplaceAll(r.detail, "|", "/"))
	}
	t.Log("\n" + b.String())
}

func rawClient(t *testing.T, ep, ak, sk string) *minio.Client {
	t.Helper()
	c, err := minio.New(ep, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
