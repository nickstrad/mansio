package objstore_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/faultnet"
	"github.com/nickstrad/mansio/server/internal/logx"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/objstore/s3test"
)

// The Release It guide H4 failure table, run against the adapter through s3test (protocol faults) and
// faultnet (transport faults). Timeouts are scaled down so the table runs in seconds.

const (
	opTimeout    = 300 * time.Millisecond
	stallTimeout = 200 * time.Millisecond
	// loadSlack bounds "did not hang" checks. Under go test -race ./... with other agents' builds the
	// scheduler can lose a second; the precise claims are made by lower bounds and attempt counts instead.
	loadSlack = 3 * time.Second
)

type rig struct {
	fake  *s3test.Server
	proxy *faultnet.Proxy
	st    *objstore.Store
	d     *objstore.Durable
	cfg   objstore.Config
}

// newRig starts a fake S3 behind a faultnet proxy and an adapter pointed at the proxy.
func newRig(t *testing.T, cfg objstore.Config) *rig {
	t.Helper()
	fake := s3test.New()
	t.Cleanup(fake.Close)
	p, err := faultnet.New(fake.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	base := fakeConfig(p.Addr())
	cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Bucket = base.Endpoint, base.AccessKey, base.SecretKey, base.Bucket
	if cfg.OpTimeout == 0 {
		cfg.OpTimeout = opTimeout
	}
	if cfg.StallTimeout == 0 {
		cfg.StallTimeout = stallTimeout
	}
	st := mustStore(t, cfg)
	return &rig{fake: fake, proxy: p, st: st, d: objstore.NewDurable(st, ""), cfg: cfg}
}

// fresh returns a new client on the proxy, so no request can reuse a pooled connection opened before a
// fault was set (faultnet faults apply to new connections).
func (r *rig) fresh(t *testing.T) *objstore.Durable {
	t.Helper()
	return objstore.NewDurable(mustStore(t, r.cfg), "")
}

// seed stores generation 1 of key with data and publishes it; returns the manifest.
func (r *rig) seed(t *testing.T, key string, data []byte) durable.Manifest {
	t.Helper()
	path, dg := srcFile(t, data)
	ctx := context.Background()
	if err := r.d.PutGeneration(ctx, durable.NextManifest(key, 1, 1, dg, time.Now()), path); err != nil {
		t.Fatal(err)
	}
	m := durable.NextManifest(key, 1, 1, dg, time.Now())
	if _, err := r.d.PublishManifest(ctx, m, durable.NoVersion); err != nil {
		t.Fatal(err)
	}
	r.fake.ResetRequests()
	return m
}

func noPart(t *testing.T, path string) {
	t.Helper()
	for _, p := range []string{path, path + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind (stat err %v)", p, err)
		}
	}
}

func wantKind(t *testing.T, err error, k objstore.Kind) {
	t.Helper()
	if got := objstore.Classify(err); got != k {
		t.Fatalf("Classify(%v) = %v, want %v", err, got, k)
	}
}

func body(n int) []byte { return bytes.Repeat([]byte("0123456789abcdef"), n/16+1)[:n] }

// Status(503) once, then healthy: a GET succeeds on the second request; a conditional PUT is sent once and
// surfaces Transient (OS-01 acceptance).
func TestOne503RetriesGetsButNotPuts(t *testing.T) {
	r := newRig(t, objstore.Config{})
	m := r.seed(t, "k", []byte("gen1"))

	r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: 503, Times: 1})
	if _, _, err := r.d.ReadManifest(context.Background(), "k"); err != nil {
		t.Fatalf("GET after one 503: %v", err)
	}
	if n := r.fake.Count("GET"); n != 2 {
		t.Fatalf("GET requests = %d, want 2", n)
	}

	r.fake.ResetRequests()
	_, v, _ := r.d.ReadManifest(context.Background(), "k")
	r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: 503, Method: "PUT", Times: 1})
	next := m
	next.Epoch = 2
	_, err := r.d.PublishManifest(context.Background(), next, v)
	wantKind(t, err, objstore.Transient)
	if errors.Is(err, durable.ErrStale) {
		t.Fatal("a 503 on publish was reported as stale")
	}
	if n := r.fake.Count("PUT"); n != 1 {
		t.Fatalf("PUT requests = %d, want exactly 1", n)
	}
}

// Status(503)×N: the one retry layer sends at most 5 requests per GET (minio-go adds none) and gives up
// with Transient; a PUT is always one request.
func TestStatusStormRequestCounts(t *testing.T) {
	for _, tc := range []struct {
		status, times, wantGets int
		ok                      bool
	}{
		{503, 4, 5, true},  // on point: the 5th attempt succeeds
		{503, 5, 5, false}, // off point: 5 attempts, all fail
		{503, 0, 5, false}, // every request fails
		{500, 2, 3, true},
		{429, 1, 2, true},
	} {
		r := newRig(t, objstore.Config{})
		r.seed(t, "k", []byte("gen1"))
		r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: tc.status, Times: tc.times})
		_, _, err := r.d.ReadManifest(context.Background(), "k")
		if (err == nil) != tc.ok {
			t.Fatalf("%d×%d: err = %v", tc.status, tc.times, err)
		}
		if err != nil {
			wantKind(t, err, objstore.Transient)
		}
		if n := r.fake.Count(""); n != tc.wantGets {
			t.Fatalf("%d×%d: %d requests, want %d", tc.status, tc.times, n, tc.wantGets)
		}
		r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: tc.status})
		r.fake.ResetRequests()
		path, _ := srcFile(t, []byte("x"))
		err = r.d.PutGeneration(context.Background(), durable.NextManifest("k", 2, 1, durabletest.DigestOf([]byte("x")), time.Now()), path)
		wantKind(t, err, objstore.Transient)
		if n := r.fake.Count("PUT"); n != 1 {
			t.Fatalf("%d storm: %d PUTs, want 1", tc.status, n)
		}
	}
}

// Non-transient answers are not retried.
func TestNotFoundAndPermanentAreNotRetried(t *testing.T) {
	r := newRig(t, objstore.Config{})
	_, _, err := r.d.ReadManifest(context.Background(), "none")
	if !errors.Is(err, durable.ErrNoManifest) {
		t.Fatalf("err = %v", err)
	}
	wantKind(t, err, objstore.NotFound)
	r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: 403, Code: "AccessDenied"})
	_, _, err = r.d.ReadManifest(context.Background(), "none")
	wantKind(t, err, objstore.Permanent)
	if n := r.fake.Count("GET"); n != 2 {
		t.Fatalf("%d GETs for one 404 and one 403, want 2", n)
	}
}

// HeadersThenStall: the fetch fails at the stall timeout, not at the size-derived deadline, and leaves no
// .part (RI-01 acceptance). Bounded by the caller's deadline, it fails within StallTimeout + 1 s.
func TestHeadersThenStallFailsAtTheStallTimeout(t *testing.T) {
	r := newRig(t, objstore.Config{OpTimeout: 5 * time.Second})
	m := r.seed(t, "k", body(4096))
	r.fake.Inject(s3test.Fault{Mode: s3test.HeadersThenStall})
	path := filepath.Join(t.TempDir(), "db")
	ctx, cancel := context.WithTimeout(context.Background(), stallTimeout+time.Second)
	defer cancel()
	t0 := time.Now()
	_, err := r.d.FetchGeneration(ctx, m, path)
	if err == nil {
		t.Fatal("fetch succeeded on a stalled body")
	}
	if d := time.Since(t0); d > stallTimeout+time.Second+200*time.Millisecond {
		t.Fatalf("fetch took %v, want <= stall timeout + 1s", d)
	}
	if !errors.Is(err, objstore.ErrTimeout) && objstore.Classify(err) != objstore.Canceled {
		t.Fatalf("err = %v, want a stall timeout", err)
	}
	noPart(t, path)

	// One stall, then healthy: the retry layer recovers.
	r.fake.Inject(s3test.Fault{Mode: s3test.HeadersThenStall, Times: 1})
	if _, err := r.d.FetchGeneration(context.Background(), m, path); err != nil {
		t.Fatalf("fetch after one stall: %v", err)
	}
}

// Dribble: a body that keeps moving (so the stall detector never fires) still fails at its size-derived
// deadline, OpTimeout + size/MinThroughput (RI-01 acceptance).
func TestDribbleFailsAtTheSizeDerivedDeadline(t *testing.T) {
	r := newRig(t, objstore.Config{StallTimeout: 5 * time.Second, MinThroughput: 1 << 20})
	data := body(1000)
	r.fake.Put("bkt", "obj", data)
	r.fake.Inject(s3test.Fault{Mode: s3test.Dribble, Rate: 100, Times: 1}) // 10 s for 1000 bytes
	path := filepath.Join(t.TempDir(), "obj")
	t0 := time.Now()
	st, err := r.st.GetToFile(context.Background(), "obj", path)
	if err != nil {
		t.Fatalf("GetToFile after one dribble: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, data) || st.Bytes != 1000 {
		t.Fatalf("got %d bytes", len(got))
	}
	if r.fake.Count("GET") != 2 {
		t.Fatalf("%d GETs, want 2 (dribble cut, then retry)", r.fake.Count("GET"))
	}
	// The cut came at ≈ OpTimeout + 1000 B / 1 MiB/s ≈ 300 ms, well before the dribble's 10 s.
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}

	os.Remove(path)
	r.fake.Inject(s3test.Fault{Mode: s3test.Dribble, Rate: 100})
	_, err = r.st.GetToFile(context.Background(), "obj2-missing-is-404-not-dribbled", path)
	wantKind(t, err, objstore.NotFound) // a dribble does not apply to an error response
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout+500*time.Millisecond)
	defer cancel()
	_, err = r.st.GetToFile(ctx, "obj", path)
	if !errors.Is(err, objstore.ErrTimeout) && objstore.Classify(err) != objstore.Canceled {
		t.Fatalf("err = %v, want the transfer deadline", err)
	}
	noPart(t, path)
}

// TruncateBody: a cut connection is Transient (retried), never ErrCorrupt, and leaves no .part.
func TestTruncatedBodyIsRetriedAndLeavesNoPart(t *testing.T) {
	r := newRig(t, objstore.Config{})
	m := r.seed(t, "k", body(100_000))
	path := filepath.Join(t.TempDir(), "db")
	r.fake.Inject(s3test.Fault{Mode: s3test.TruncateBody, Times: 2})
	if _, err := r.d.FetchGeneration(context.Background(), m, path); err != nil {
		t.Fatalf("fetch after two truncations: %v", err)
	}
	if n := r.fake.Count("GET"); n != 3 {
		t.Fatalf("%d GETs, want 3", n)
	}
	os.Remove(path)
	r.fake.Inject(s3test.Fault{Mode: s3test.TruncateBody})
	_, err := r.d.FetchGeneration(context.Background(), m, path)
	wantKind(t, err, objstore.Transient)
	if errors.Is(err, durable.ErrCorrupt) {
		t.Fatal("a truncated transfer was reported as a corrupt object")
	}
	noPart(t, path)
}

// CorruptBody / WrongContentLength: the bytes arrived but are wrong: ErrCorrupt, not retried, no file.
func TestCorruptBodyIsErrCorruptAndNotRetried(t *testing.T) {
	for _, mode := range []s3test.Mode{s3test.CorruptBody, s3test.WrongContentLength} {
		r := newRig(t, objstore.Config{})
		m := r.seed(t, "k", body(5000))
		r.fake.Inject(s3test.Fault{Mode: mode})
		path := filepath.Join(t.TempDir(), "db")
		_, err := r.d.FetchGeneration(context.Background(), m, path)
		if !errors.Is(err, durable.ErrCorrupt) {
			t.Fatalf("%v: err = %v, want ErrCorrupt", mode, err)
		}
		wantKind(t, err, objstore.Permanent)
		if n := r.fake.Count("GET"); n != 1 {
			t.Fatalf("%v: %d GETs, want 1", mode, n)
		}
		noPart(t, path)
	}
}

// ApplyThenDropReply: the publish applied but its reply was lost. The adapter sends it once and reports an
// ambiguous error, never ErrStale (the G2c misreport minio-go's retries used to cause); a re-read settles it.
func TestLostPublishReplyIsAmbiguousNotStale(t *testing.T) {
	r := newRig(t, objstore.Config{})
	m := r.seed(t, "k", []byte("gen1"))
	_, v, _ := r.d.ReadManifest(context.Background(), "k")
	next := m
	next.Epoch = 2
	r.fake.ResetRequests()
	r.fake.Inject(s3test.Fault{Mode: s3test.ApplyThenDropReply, Times: 1})
	_, err := r.d.PublishManifest(context.Background(), next, v)
	if err == nil || errors.Is(err, durable.ErrStale) {
		t.Fatalf("err = %v, want an ambiguous error", err)
	}
	wantKind(t, err, objstore.Transient)
	if n := r.fake.Count("PUT"); n != 1 {
		t.Fatalf("%d PUTs, want 1", n)
	}
	if got, _, err := r.d.ReadManifest(context.Background(), "k"); err != nil || !got.Same(next) {
		t.Fatalf("re-read: %+v, %v; the publish should have applied", got, err)
	}
}

// faultnet.Refuse: every attempt fails at once; the caller's deadline stops the retries.
func TestRefusedConnectionsFailFast(t *testing.T) {
	r := newRig(t, objstore.Config{})
	r.seed(t, "k", []byte("gen1"))
	r.proxy.Set(faultnet.RefuseConn(), 0)
	d := r.fresh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, _, err := d.ReadManifest(ctx, "k")
	if k := objstore.Classify(err); k != objstore.Transient && k != objstore.Canceled {
		t.Fatalf("Classify(%v) = %v", err, k)
	}
	if d := time.Since(t0); d > 150*time.Millisecond {
		t.Fatalf("refused read took %v under a 100ms deadline", d)
	}
	before := r.proxy.Accepted()
	_, _, err = d.ReadManifest(context.Background(), "k")
	wantKind(t, err, objstore.Transient)
	if n := r.proxy.Accepted() - before; n != 5 {
		t.Fatalf("%d connections, want one per attempt (5)", n)
	}
}

// faultnet.Blackhole / AcceptThenSilence: each attempt fails at the op timeout.
func TestSilentPeerFailsAtTheOpTimeout(t *testing.T) {
	for _, f := range []faultnet.Fault{faultnet.Hole(), faultnet.Silence()} {
		r := newRig(t, objstore.Config{})
		r.seed(t, "k", []byte("gen1"))
		r.proxy.Set(f, 1) // the first new connection is dead; the retry dials a healthy one
		d := r.fresh(t)
		t0 := time.Now()
		if _, _, err := d.ReadManifest(context.Background(), "k"); err != nil {
			t.Fatalf("%v: %v", f.Kind, err)
		}
		if el := time.Since(t0); el < opTimeout || el > opTimeout+loadSlack {
			t.Fatalf("%v: read took %v, want one op timeout (%v) plus a backoff", f.Kind, el, opTimeout)
		}
		// Exactly one timed-out attempt, then one that succeeded: what the elapsed time used to show.
		if got := d.RequestCounts()[objstore.OpGetManifest]; got != 2 {
			t.Fatalf("%v: %d attempts, want 2 (one timeout, one retry)", f.Kind, got)
		}
	}
}

// faultnet.HalfOpen: an idle pooled connection silently dies (a NAT forgot it). The next request on it
// fails at the op timeout and the retry uses a new connection: no hang.
func TestHalfOpenPooledConnectionIsRecycled(t *testing.T) {
	r := newRig(t, objstore.Config{})
	r.seed(t, "k", []byte("gen1")) // leaves a pooled connection
	r.proxy.Set(faultnet.Frozen(), 0)
	r.proxy.Heal()
	t0 := time.Now()
	if _, _, err := r.d.ReadManifest(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > opTimeout+loadSlack {
		t.Fatalf("read took %v", d)
	}
}

// A PUT into a silent peer fails at its deadline, is sent once, and says it may have applied.
func TestSilentPutIsSentOnceAndFailsAtTheDeadline(t *testing.T) {
	r := newRig(t, objstore.Config{})
	r.proxy.Set(faultnet.Silence(), 0)
	d := r.fresh(t)
	path, _ := srcFile(t, []byte("x"))
	before := r.proxy.Accepted()
	t0 := time.Now()
	err := d.PutGeneration(context.Background(), durable.NextManifest("k", 1, 1, durabletest.DigestOf([]byte("x")), time.Now()), path)
	wantKind(t, err, objstore.Transient)
	if el := time.Since(t0); el < opTimeout || el > opTimeout+loadSlack {
		t.Fatalf("PUT failed after %v, want ≈ %v: %v", el, opTimeout, err)
	}
	if n := r.proxy.Accepted() - before; n != 1 {
		t.Fatalf("%d connections, want 1: a PUT is never retried", n)
	}
}

// Against SeaweedFS through faultnet (gated): a connection reset mid-body is retried, a refused store is
// Transient, and nothing is left on disk.
func TestS3TransportFaults(t *testing.T) {
	ep := s3Endpoint(t)
	p, err := faultnet.New(ep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	st := s3Store(t, p.Addr())
	root := "test-" + logx.NewID() + "/"
	t.Cleanup(func() { s3Store(t, ep).DeletePrefix(context.Background(), root) })
	d := objstore.NewDurable(st, root)
	data := body(1 << 20)
	path, dg := srcFile(t, data)
	if err := d.PutGeneration(context.Background(), durable.NextManifest("k", 1, 1, dg, time.Now()), path); err != nil {
		t.Fatal(err)
	}
	m := durable.NextManifest("k", 1, 1, dg, time.Now())

	fresh := objstore.NewDurable(mustStore(t, s3Config(p.Addr())), root)
	p.Set(faultnet.ResetAfterBytes(64<<10), 1)
	before := p.Accepted()
	out := filepath.Join(t.TempDir(), "db")
	if _, err := fresh.FetchGeneration(context.Background(), m, out); err != nil {
		t.Fatalf("fetch after one mid-body reset: %v", err)
	}
	if n := p.Accepted() - before; n < 2 {
		t.Fatalf("%d connections: the reset did not happen or was not retried", n)
	}

	p.Set(faultnet.RefuseConn(), 0)
	_, err = objstore.NewDurable(mustStore(t, s3Config(p.Addr())), root).FetchGeneration(context.Background(), m, out+"2")
	wantKind(t, err, objstore.Transient)
	noPart(t, out+"2")
}

// OS-07: a byte flipped in flight makes the server reject the PUT (Content-MD5 → BadDigest, Permanent), and
// no object exists afterwards. The flip lands in the body: 32 KiB into the request, past the headers.
func TestFlippedUploadByteIsRejectedByTheServer(t *testing.T) {
	r := newRig(t, objstore.Config{})
	data := bytes.Repeat([]byte("a"), 64<<10)
	path, _ := srcFile(t, data)
	r.proxy.Set(faultnet.FlipByteAt(32<<10), 0)
	d := r.fresh(t)
	err := d.PutGeneration(context.Background(), durable.NextManifest("k", 1, 1, durabletest.DigestOf(data), time.Now()), path)
	wantKind(t, err, objstore.Permanent)
	if _, ok := r.fake.Object("bkt", durable.GenObject("k", 1, 1)); ok {
		t.Fatal("a corrupted upload was stored")
	}
	r.proxy.Heal()
	if err := r.fresh(t).PutGeneration(context.Background(), durable.NextManifest("k", 1, 1, durabletest.DigestOf(data), time.Now()), path); err != nil {
		t.Fatalf("clean upload after the flip: %v", err)
	}
}

// Against SeaweedFS (gated): the same flip is rejected and nothing is stored.
func TestS3FlippedUploadByteIsRejected(t *testing.T) {
	ep := s3Endpoint(t)
	p, err := faultnet.New(ep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	root := "test-" + logx.NewID() + "/"
	direct := objstore.NewDurable(s3Store(t, ep), root)
	t.Cleanup(func() { s3Store(t, ep).DeletePrefix(context.Background(), root) })
	data := bytes.Repeat([]byte("a"), 64<<10)
	path, _ := srcFile(t, data)
	p.Set(faultnet.FlipByteAt(32<<10), 0)
	err = objstore.NewDurable(mustStore(t, s3Config(p.Addr())), root).PutGeneration(context.Background(), durable.NextManifest("k", 1, 1, durabletest.DigestOf(data), time.Now()), path)
	// SeaweedFS 4.47 answers 500 InternalError here (Transient), not BadDigest: the flipped byte also breaks
	// the chunk signature of the streaming-signed body. Either way it is rejected and the PUT is not re-sent.
	if err == nil {
		t.Fatal("a corrupted upload was accepted")
	}
	t.Logf("SeaweedFS answer to a flipped body byte (%v): %v", objstore.Classify(err), err)
	m := durable.NextManifest("k", 1, 1, durabletest.DigestOf(data), time.Now())
	if _, err := direct.FetchGeneration(context.Background(), m, filepath.Join(t.TempDir(), "db")); !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("after a rejected upload, fetch err = %v, want ErrNotFound", err)
	}
}

// The upload's Content-MD5 comes from the caller's digest (computed in the same pass as the sha256): a wrong
// one is rejected by the server, so the adapter did not re-read the file for it; without one, the adapter
// computes it.
func TestContentMD5ComesFromTheDigest(t *testing.T) {
	r := newRig(t, objstore.Config{})
	data := []byte("generation")
	path, dg := srcFile(t, data)
	bad := dg
	bad.MD5 = durabletest.DigestOf([]byte("other")).MD5
	err := r.d.PutGeneration(context.Background(), durable.NextManifest("k", 1, 1, bad, time.Now()), path)
	wantKind(t, err, objstore.Permanent)
	none := dg
	none.MD5 = ""
	if err := r.d.PutGeneration(context.Background(), durable.NextManifest("k", 1, 1, none, time.Now()), path); err != nil {
		t.Fatalf("upload without a digest MD5: %v", err)
	}
	if err := r.d.PutGeneration(context.Background(), durable.NextManifest("k", 2, 1, dg, time.Now()), path); err != nil {
		t.Fatalf("upload with the digest MD5: %v", err)
	}
}

// Size is one HEAD, retried while Transient; a missing object is ErrNotFound (coldload's Sizer).
func TestSizeHeadsAndRetries(t *testing.T) {
	r := newRig(t, objstore.Config{})
	r.fake.Put("bkt", "obj", body(1234))
	r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: 503, Times: 1})
	n, err := r.st.Size(context.Background(), "obj")
	if err != nil || n != 1234 {
		t.Fatalf("Size = %d, %v; want 1234", n, err)
	}
	if r.fake.Count("HEAD") != 2 {
		t.Fatalf("%d HEADs, want 2 (503, then retry)", r.fake.Count("HEAD"))
	}
	if _, err := r.st.Size(context.Background(), "missing"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Size(missing) = %v, want ErrNotFound", err)
	}
}
