package objstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/logx"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/objstore/s3test"
)

// requests is the number of attempts per op, ops with none left out.
func requests(st *objstore.Store) map[string]int64 {
	out := map[string]int64{}
	for op, o := range st.Metrics() {
		if n := o.Requests(); n > 0 {
			out[op] = n
		}
	}
	return out
}

func diff(after, before map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for op, n := range after {
		if d := n - before[op]; d > 0 {
			out[op] = d
		}
	}
	return out
}

func sameCounts(t *testing.T, what string, got, want map[string]int64) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Fatalf("%s: requests %v, want %v", what, got, want)
	}
}

// The request accounting of object-storage plan G7, checked on a scripted write-back, stamp and activation
// against the fake: the counters must match what the server saw (OS-08 acceptance).
func TestMetricsCountRequestsPerLogicalOperation(t *testing.T) {
	fake := s3test.New()
	t.Cleanup(fake.Close)
	st := mustStore(t, fakeConfig(fake.Endpoint()))
	d := objstore.NewDurable(st, "")
	ctx := context.Background()
	data := []byte("generation one")
	path, dg := srcFile(t, data)

	// Write-back: PUT generation, HEAD, GET manifest (pre-read), PUT manifest = 4.
	before := requests(st)
	if err := d.PutGeneration(ctx, "k", 1, path, dg); err != nil {
		t.Fatal(err)
	}
	m := durable.NextManifest("k", 1, 1, dg, time.Now())
	if _, err := d.PublishManifest(ctx, m, durable.NoVersion); err != nil {
		t.Fatal(err)
	}
	sameCounts(t, "write-back", diff(requests(st), before),
		map[string]int64{objstore.OpPutGen: 1, objstore.OpHead: 1, objstore.OpGetManifest: 1, objstore.OpPublish: 1})
	if n := fake.Count(""); n != 4 {
		t.Fatalf("server saw %d requests for a write-back, want 4", n)
	}

	// Stamp: GET in durable.Stamp, GET pre-read, PUT = 3.
	before = requests(st)
	if _, _, err := durable.Stamp(ctx, d, "k", 2, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	sameCounts(t, "stamp", diff(requests(st), before), map[string]int64{objstore.OpGetManifest: 2, objstore.OpPublish: 1})

	// Activation: GET manifest, GET generation = 2.
	before = requests(st)
	cur, _, err := d.ReadManifest(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.FetchGeneration(ctx, cur, filepath.Join(t.TempDir(), "db")); err != nil {
		t.Fatal(err)
	}
	sameCounts(t, "activation", diff(requests(st), before), map[string]int64{objstore.OpGetManifest: 1, objstore.OpFetch: 1})

	got := st.Metrics()
	if got[objstore.OpFetch].Bytes != int64(len(data)) || got[objstore.OpPutGen].Bytes != int64(len(data)) {
		t.Fatalf("bytes: fetch %d, put_gen %d, want %d", got[objstore.OpFetch].Bytes, got[objstore.OpPutGen].Bytes, len(data))
	}
	for op, o := range got {
		var sum int64
		for _, c := range o.Latency {
			sum += c
		}
		if sum != o.Requests() {
			t.Fatalf("%s: latency histogram holds %d, requests %d", op, sum, o.Requests())
		}
		if len(o.Latency) != len(objstore.LatencyBuckets)+1 {
			t.Fatalf("%s: %d latency buckets", op, len(o.Latency))
		}
	}
}

// Retried and failed attempts are counted by kind; each retried one is logged once with its op and object.
func TestMetricsCountAndLogRetries(t *testing.T) {
	fake := s3test.New()
	t.Cleanup(fake.Close)
	var logs bytes.Buffer
	st, err := objstore.New(objstore.Config{Endpoint: fake.Endpoint(), AccessKey: "a", SecretKey: "b", Bucket: "bkt",
		Logger: logx.New(&logs, slog.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	d := objstore.NewDurable(st, "")
	fake.Inject(s3test.Fault{Mode: s3test.Status, Status: 503, Times: 2})
	_, _, err = d.ReadManifest(context.Background(), "k") // 503, 503, 404
	if objstore.Classify(err) != objstore.NotFound {
		t.Fatalf("err = %v", err)
	}
	o := st.Metrics()[objstore.OpGetManifest]
	if o.ByKind[objstore.Transient] != 2 || o.ByKind[objstore.NotFound] != 1 || o.Retried != 2 || o.Requests() != 3 {
		t.Fatalf("get_manifest stats %+v", o)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want one per retried attempt:\n%s", len(lines), logs.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "S3.REQUEST_RETRIED" || rec["op"] != objstore.OpGetManifest || rec["kind"] != "transient" ||
		rec["object"] != durable.ManifestObject("k") || rec["level"] != "WARN" {
		t.Fatalf("log line %v", rec)
	}
}
