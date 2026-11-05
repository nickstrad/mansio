package vactor_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/panicx"
	"github.com/nickstrad/ts-db-go/internal/vactor"
	"github.com/nickstrad/ts-db-go/internal/vactor/vactortest"
)

// clock is a settable time source for Options.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type drig struct {
	h     *vactor.Host
	s     *durabletest.Store
	o     *vactortest.Opener
	clk   *clock
	dir   string
	opts  vactor.Options
	t     *testing.T
	ended bool
}

func newDurable(t *testing.T, mod func(*vactor.Options)) *drig {
	t.Helper()
	r := &drig{s: durabletest.NewStore(), o: &vactortest.Opener{}, clk: &clock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)},
		dir: filepath.Join(t.TempDir(), "cache"), t: t}
	r.opts = vactor.Options{CacheDir: r.dir, Store: r.s, CreateMissing: true, Now: r.clk.Now}
	if mod != nil {
		mod(&r.opts)
	}
	r.restart()
	t.Cleanup(func() {
		if !r.ended {
			r.s.SetFaults(durabletest.Faults{})
			r.o.Set(false, false)
			r.o.SetFailSnapshot(false)
			r.h.Close()
		}
	})
	return r
}

// restart replaces the host with a fresh one on the same store (a process restart: local files gone).
func (r *drig) restart() {
	r.t.Helper()
	h, err := vactor.NewHost(nil, r.o, r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	r.h = h
}

func (r *drig) get(key string) vactor.DB {
	r.t.Helper()
	res, err := r.h.Get(ctx, key)
	if err != nil {
		r.t.Fatalf("Get(%s): %v", key, err)
	}
	return res.DB
}

func (r *drig) put(key, value string) {
	r.t.Helper()
	if err := r.get(key).Query(ctx, vactortest.Put(value)); err != nil {
		r.t.Fatal(err)
	}
}

// local reads key's local file (the fake db's contents).
func (r *drig) local(key string) string {
	r.t.Helper()
	b, err := os.ReadFile(r.h.LocalPath(key))
	if err != nil {
		r.t.Fatalf("local copy of %s: %v", key, err)
	}
	return string(b)
}

// published is the contents of key's published generation ("" for none / generation 0).
func (r *drig) published(key string) string {
	r.t.Helper()
	m, ok := r.s.Manifest(key)
	if !ok || m.Generation == 0 {
		return ""
	}
	b, ok := r.s.Object(m.Object)
	if !ok {
		r.t.Fatalf("manifest of %s names missing %s", key, m.Object)
	}
	return string(b)
}

func mustModule(t *testing.T, err error, sub string) {
	t.Helper()
	e, ok := errs.Outermost(err)
	if !ok || e.Module != "vactor" {
		t.Fatalf("error is not a well-formed vactor error: %v", err)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Fatalf("error %q does not say %q", err, sub)
	}
}

func TestNewHostValidatesDurableOptions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	s, o := durabletest.NewStore(), &vactortest.Opener{}
	for name, opts := range map[string]vactor.Options{
		"no fetcher, no store": {CacheDir: dir},
		"keep local + store":   {CacheDir: dir, Store: s, KeepLocal: true},
		"negative policy":      {CacheDir: dir, Store: s, Policy: vactor.Policy{IdleAfter: -1}},
	} {
		if _, err := vactor.NewHost(nil, o, opts); err == nil {
			t.Errorf("%s: NewHost succeeded", name)
		}
	}
}

func TestActivationFetchesTheManifestsGeneration(t *testing.T) {
	r := newDurable(t, nil)
	r.s.Seed("t/a", 0, []byte("v1"))
	m := r.s.Seed("t/a", 0, []byte("v2"))
	res, err := r.h.Get(ctx, "t/a")
	if err != nil || res.Act == nil || res.Act.Bytes != 2 {
		t.Fatalf("Get = %+v, %v", res, err)
	}
	if got := r.local("t/a"); got != "v2" {
		t.Fatalf("activated %q, want the manifest's generation %d (v2)", got, m.Generation)
	}
}

func TestActivationWithoutManifest(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.CreateMissing = false })
	_, err := r.h.Get(ctx, "t/none")
	if !errors.Is(err, vactor.ErrNotFound) || !errors.Is(err, durable.ErrNoManifest) {
		t.Fatalf("err = %v, want ErrNotFound and ErrNoManifest", err)
	}
	r2 := newDurable(t, nil) // CreateMissing: an empty database, published on first dirty write-back
	if got := r2.getLocal("t/new"); got != "" {
		t.Fatalf("new database file = %q, want empty", got)
	}
	if err := r2.h.Passivate(ctx, "t/new"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r2.s.Manifest("t/new"); ok {
		t.Fatal("a clean new database was published")
	}
}

// getLocal activates key and returns its local contents.
func (r *drig) getLocal(key string) string { r.get(key); return r.local(key) }

func TestActivationRejectsBadKeysAndLowerEpochs(t *testing.T) {
	r := newDurable(t, nil)
	if _, err := r.h.Get(ctx, "bad//key"); err == nil {
		t.Fatal("Get with an invalid key succeeded")
	}
	r.s.Seed("t/a", 5, []byte("v"))
	if _, err := r.h.GetAt(ctx, "t/a", 4); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("GetAt below the stored epoch: %v, want ErrStale", err)
	}
	if _, err := r.h.GetAt(ctx, "t/a", 5); err != nil { // equal epoch: on point
		t.Fatal(err)
	}
}

func TestMissingOrCorruptGenerationFailsActivation(t *testing.T) {
	r := newDurable(t, nil)
	m := r.s.Seed("t/a", 0, []byte("v"))
	r.s.DeleteObject(m.Object)
	if _, err := r.h.Get(ctx, "t/a"); !errors.Is(err, vactor.ErrNotFound) || !errors.Is(err, durable.ErrNotFound) {
		t.Fatalf("missing generation: %v", err)
	}
	m = r.s.Seed("t/b", 0, []byte("v"))
	r.s.Corrupt(m.Object)
	if _, err := r.h.Get(ctx, "t/b"); !errors.Is(err, durable.ErrCorrupt) {
		t.Fatalf("corrupt generation: %v", err)
	}
	if _, err := os.Stat(r.h.LocalPath("t/b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a corrupt fetch left a local file")
	}
	r.s.SetFaults(durabletest.Faults{FailRead: true})
	if _, err := r.h.Get(ctx, "t/c"); err == nil || !strings.Contains(err.Error(), "manifest read failed") {
		t.Fatalf("manifest read failure: %v", err)
	}
	r.s.SetFaults(durabletest.Faults{FailFetch: true})
	if _, err := r.h.Get(ctx, "t/a2"); err != nil { // CreateMissing: no fetch for a new key
		t.Fatal(err)
	}
	r.s.Seed("t/d", 0, []byte("v"))
	if _, err := r.h.Get(ctx, "t/d"); !errors.Is(err, durabletest.ErrInjected) {
		t.Fatalf("fetch failure: %v", err)
	}
}

func TestPassivateWritesBackOnlyWhenDirty(t *testing.T) {
	r := newDurable(t, nil)
	r.s.Seed("t/a", 0, []byte("v1"))
	r.get("t/a")
	if err := r.h.Passivate(ctx, "t/a"); err != nil {
		t.Fatal(err)
	}
	if puts, _, _ := r.s.Counts(); puts != 0 {
		t.Fatalf("clean passivation uploaded %d generations", puts)
	}
	r.put("t/a", "v2")
	if err := r.h.Passivate(ctx, "t/a"); err != nil {
		t.Fatal(err)
	}
	if got := r.published("t/a"); got != "v2" {
		t.Fatalf("published %q after a dirty passivation, want v2", got)
	}
	if _, err := os.Stat(r.h.LocalPath("t/a")); !errors.Is(err, os.ErrNotExist) || r.h.Active() != 0 {
		t.Fatal("passivation left the database active or its file on disk")
	}
	if m, _ := r.s.Manifest("t/a"); m.Generation != 2 || m.Size != 2 {
		t.Fatalf("manifest %+v", m)
	}
	if s := r.h.Stats(); s.WriteBacks != 1 || s.BytesUploaded != 2 {
		t.Fatalf("Stats %+v", s)
	}
	if got := r.getLocal("t/a"); got != "v2" {
		t.Fatalf("reactivated %q", got)
	}
}

// OS-08: Host.Stats reports the store's request counts and their cost. An activation is one manifest read and
// one fetch; a warm write-back is four requests (put_gen + head + get_manifest + publish, guide G7).
func TestStatsCountRequestsPerWriteBack(t *testing.T) {
	r := newDurable(t, nil)
	r.s.Seed("t/a", 0, []byte("v1"))
	r.put("t/a", "v2")
	act := r.h.Stats().Requests
	if want := map[string]int64{durable.OpGetManifest: 1, durable.OpFetch: 1}; !maps.Equal(act, want) {
		t.Fatalf("after activation Requests = %v, want %v", act, want)
	}
	if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
		t.Fatal(err)
	}
	st := r.h.Stats()
	wb := durable.SubCounts(st.Requests, act)
	want := map[string]int64{durable.OpPutGen: 1, durable.OpHead: 1, durable.OpGetManifest: 1, durable.OpPublish: 1}
	if !maps.Equal(wb, want) {
		t.Fatalf("one write-back sent %v, want %v", wb, want)
	}
	// 2 class A (PUTs) + 4 class B (2 manifest GETs, fetch, HEAD).
	if cost, want := st.RequestCostUSD, 2*durable.S3Standard.ClassA+4*durable.S3Standard.ClassB; math.Abs(cost-want) > 1e-15 {
		t.Fatalf("RequestCostUSD = %g, want %g", cost, want)
	}
}

func TestFailedWriteBackKeepsTheDatabaseWarm(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    durabletest.Faults
		say  string
	}{
		{"upload fails mid-way", durabletest.Faults{FailUpload: true}, "upload of generation 2 failed"},
		{"crash between upload and publish", durabletest.Faults{FailPublish: true}, "publishing generation 2 failed"},
		{"store down", durabletest.Faults{Down: true}, "safe to retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDurable(t, nil)
			r.s.Seed("t/a", 0, []byte("v1"))
			r.put("t/a", "v2")
			r.s.SetFaults(tc.f)
			err := r.h.Passivate(ctx, "t/a")
			mustModule(t, err, tc.say)
			mustModule(t, err, "nothing deleted")
			if r.h.Active() != 1 || r.local("t/a") != "v2" || r.published("t/a") != "v1" {
				t.Fatalf("after a failed passivation: active=%d local=%q published=%q", r.h.Active(), r.local("t/a"), r.published("t/a"))
			}
			// Warm again: it serves and accepts writes, then a retry publishes them.
			r.put("t/a", "v3")
			r.s.SetFaults(durabletest.Faults{})
			if err := r.h.Passivate(ctx, "t/a"); err != nil {
				t.Fatal(err)
			}
			if got := r.published("t/a"); got != "v3" {
				t.Fatalf("published %q after retry, want v3", got)
			}
		})
	}
}

func TestOrphanGenerationIsSkippedNotOverwritten(t *testing.T) {
	r := newDurable(t, nil)
	r.s.Seed("t/a", 0, []byte("v1"))
	r.put("t/a", "v2")
	r.s.SetFaults(durabletest.Faults{FailPublish: true})
	if _, err := r.h.WriteBack(ctx, "t/a"); err == nil {
		t.Fatal("WriteBack with a failing publish succeeded")
	}
	orphan := durable.GenObject("t/a", 2, 0)
	if b, ok := r.s.Object(orphan); !ok || string(b) != "v2" {
		t.Fatal("expected an orphan generation 2")
	}
	r.s.SetFaults(durabletest.Faults{})
	r.restart() // a new process: does not know about the orphan
	r.put("t/a", "v3")
	if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.s.Manifest("t/a"); m.Generation != 3 || r.published("t/a") != "v3" {
		t.Fatalf("manifest %+v (published %q); want generation 3 skipping the orphan", m, r.published("t/a"))
	}
	if b, _ := r.s.Object(orphan); string(b) != "v2" {
		t.Fatal("the orphan was overwritten")
	}
}

func TestLostPublishReplyIsReconciled(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	r.s.SetFaults(durabletest.Faults{LosePublishReply: true})
	res, err := r.h.WriteBack(ctx, "t/a")
	if err != nil || !res.Wrote || r.published("t/a") != "v1" {
		t.Fatalf("WriteBack with a lost reply = %+v, %v; the re-read should find our manifest", res, err)
	}
	// Lost reply and the re-read fails too: ambiguous. The next write-back must find our earlier publish and
	// build on it, not report ErrStale against ourselves.
	r.put("t/a", "v2")
	r.s.SetFaults(durabletest.Faults{LosePublishReply: true, FailRead: true})
	if _, err := r.h.WriteBack(ctx, "t/a"); err == nil || errors.Is(err, durable.ErrStale) {
		t.Fatalf("ambiguous publish: %v", err)
	}
	r.s.SetFaults(durabletest.Faults{})
	r.put("t/a", "v3")
	if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
		t.Fatalf("write-back after an ambiguous publish: %v", err)
	}
	if got := r.published("t/a"); got != "v3" {
		t.Fatalf("published %q", got)
	}
	if res, err := r.h.WriteBack(ctx, "t/a"); err != nil || res.Wrote {
		t.Fatalf("clean WriteBack = %+v, %v; want nothing to do", res, err)
	}
}

func TestHigherEpochMakesWriteBackStale(t *testing.T) {
	r := newDurable(t, nil)
	r.s.Seed("t/a", 1, []byte("v1"))
	if _, err := r.h.GetAt(ctx, "t/a", 1); err != nil {
		t.Fatal(err)
	}
	r.put("t/a", "zombie")
	r.s.Bump("t/a", 2) // the new owner stamped epoch 2
	_, err := r.h.WriteBack(ctx, "t/a")
	if !errors.Is(err, durable.ErrStale) || !strings.Contains(err.Error(), "not safe to retry") {
		t.Fatalf("WriteBack after a higher epoch: %v", err)
	}
	if err := r.h.Passivate(ctx, "t/a"); !errors.Is(err, durable.ErrStale) || r.h.Active() != 1 {
		t.Fatalf("Passivate of a stale dirty db: %v (active %d); want ErrStale and still warm", err, r.h.Active())
	}
	rep, _ := r.h.Tick(ctx) // the planner leaves it alone
	if len(rep.Plan.WriteBack)+len(rep.Plan.Passivate) != 0 {
		t.Fatalf("Tick planned %+v for a stale db", rep.Plan)
	}
	if !r.h.Discard("t/a") {
		t.Fatal("Discard did not report the lost writes")
	}
	if r.h.Active() != 0 || r.published("t/a") != "v1" {
		t.Fatal("Discard left it active or the zombie's writes got published")
	}
	if r.h.Discard("t/a") {
		t.Fatal("Discard of an inactive key reported lost writes")
	}
}

func TestGetAtRaisesThePublishEpoch(t *testing.T) {
	r := newDurable(t, nil)
	r.s.Seed("t/a", 1, []byte("v1"))
	if _, err := r.h.GetAt(ctx, "t/a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.GetAt(ctx, "t/a", 3); err != nil {
		t.Fatal(err)
	}
	r.put("t/a", "v2")
	if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.s.Manifest("t/a"); m.Epoch != 3 {
		t.Fatalf("published at epoch %d, want 3", m.Epoch)
	}
}

func TestStamp(t *testing.T) {
	r := newDurable(t, nil)
	m := r.s.Seed("t/a", 1, []byte("v1"))
	if got, err := r.h.Stamp(ctx, "t/a", 2, m.Generation); err != nil || got.Epoch != 2 {
		t.Fatalf("cold Stamp = %+v, %v", got, err)
	}
	if _, err := r.h.GetAt(ctx, "t/a", 2); err != nil {
		t.Fatal(err)
	}
	if got, err := r.h.Stamp(ctx, "t/a", 3, m.Generation); err != nil || got.Epoch != 3 {
		t.Fatalf("warm Stamp = %+v, %v", got, err)
	}
	r.put("t/a", "v2") // the stamp moved the manifest version; our own write-back must still succeed
	if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
		t.Fatalf("write-back after our own stamp: %v", err)
	}
	if m2, _ := r.s.Manifest("t/a"); m2.Epoch != 3 || m2.Generation != 2 {
		t.Fatalf("manifest %+v", m2)
	}
	if _, err := r.h.Stamp(ctx, "t/a", 3, 1); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("warm stamp at an old generation: %v", err)
	}
	r.s.Bump("t/a", 9)
	if _, err := r.h.Stamp(ctx, "t/a", 4, 2); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("warm stamp after someone else moved the manifest: %v", err)
	}
	if _, err := r.h.Stamp(ctx, "t/b", 1, 5); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("cold stamp of a missing manifest at gen 5: %v", err)
	}
	plain := newRig(t, false)
	if _, err := plain.h.Stamp(ctx, "t/a", 1, 0); err == nil {
		t.Fatal("Stamp on a host without a store succeeded")
	}
}

func TestSnapshotFailureUploadsNothing(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	r.o.SetFailSnapshot(true)
	_, err := r.h.WriteBack(ctx, "t/a")
	mustModule(t, err, "snapshot failed; nothing uploaded")
	if puts, _, _ := r.s.Counts(); puts != 0 {
		t.Fatal("uploaded after a failed snapshot")
	}
	if _, err := os.Stat(r.h.LocalPath("t/a") + ".snap"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed snapshot left its file")
	}
}

func TestStrandedWhenReopenFailsThenRecovered(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	r.s.SetFaults(durabletest.Faults{FailUpload: true})
	r.o.Set(true, false) // the reopen after the failed upload fails too
	if err := r.h.Passivate(ctx, "t/a"); err == nil {
		t.Fatal("Passivate succeeded")
	}
	r.o.Set(false, false)
	if _, err := r.h.Get(ctx, "t/a"); err == nil || !strings.Contains(err.Error(), "could not be reopened") {
		t.Fatalf("Get of a stranded db: %v; want an error, never stale data from the store", err)
	}
	if r.local("t/a") != "v1" || r.h.Active() != 1 {
		t.Fatal("the stranded copy is gone")
	}
	if _, err := r.h.WriteBack(ctx, "t/a"); err == nil {
		t.Fatal("WriteBack of a stranded db with the store failing succeeded")
	}
	r.s.SetFaults(durabletest.Faults{})
	if err := r.h.Passivate(ctx, "t/a"); err != nil {
		t.Fatalf("Passivate of a stranded db once the store is back: %v", err)
	}
	if got := r.published("t/a"); got != "v1" {
		t.Fatalf("published %q", got)
	}
}

func TestPassivateUploadsViaSnapshotWhenAWALIsLeft(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	if err := os.WriteFile(r.h.LocalPath("t/a")+"-wal", []byte("frames"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Passivate(ctx, "t/a"); err != nil {
		t.Fatal(err)
	}
	if got := r.published("t/a"); got != "v1" {
		t.Fatalf("published %q", got)
	}
}

func TestRestartRecoversTheLastPublishedGeneration(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
		t.Fatal(err)
	}
	r.put("t/a", "v2-unpublished")
	vactor.Crash(r.h)
	r.restart()
	if got := r.getLocal("t/a"); got != "v1" {
		t.Fatalf("after a crash: %q, want the last published v1", got)
	}
}

func TestCloseWritesBackAndKeepsFilesOnFailure(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	r.put("t/b", "v1")
	r.s.SetFaults(durabletest.Faults{FailUpload: true})
	err := r.h.Close()
	mustModule(t, err, "only up-to-date copies")
	if _, serr := os.Stat(r.h.LocalPath("t/a")); serr != nil {
		t.Fatalf("Close deleted an unwritten copy: %v", serr)
	}
	if _, err := r.h.Get(ctx, "t/a"); !errors.Is(err, vactor.ErrClosed) {
		t.Fatalf("Get after Close: %v", err)
	}
	r.ended = true

	r2 := newDurable(t, nil)
	r2.put("t/a", "v1")
	if err := r2.h.Close(); err != nil {
		t.Fatal(err)
	}
	r2.ended = true
	if r2.published("t/a") != "v1" {
		t.Fatal("Close did not write back")
	}
	if _, err := os.Stat(r2.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a clean Close left the cache dir")
	}
}

func TestTickWritesBackByCommitsAndByInterval(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) {
		o.Policy = vactor.Policy{WriteBackCommits: 3, WriteBackEvery: time.Minute}
	})
	r.put("t/a", "1")
	r.put("t/a", "2")
	rep, err := r.h.Tick(ctx)
	if err != nil || len(rep.WroteBack) != 0 {
		t.Fatalf("2 commits < 3 and no time passed: %+v %v", rep, err)
	}
	r.put("t/a", "3") // on point: 3 commits
	if rep, _ = r.h.Tick(ctx); len(rep.WroteBack) != 1 || r.published("t/a") != "3" {
		t.Fatalf("3 commits: %+v (published %q)", rep, r.published("t/a"))
	}
	r.put("t/a", "4")
	r.clk.Add(time.Minute - time.Nanosecond) // off point
	if rep, _ = r.h.Tick(ctx); len(rep.WroteBack) != 0 {
		t.Fatalf("just under the interval: %+v", rep)
	}
	r.clk.Add(time.Nanosecond) // on point
	if rep, _ = r.h.Tick(ctx); len(rep.WroteBack) != 1 || r.published("t/a") != "4" {
		t.Fatalf("interval reached: %+v", rep)
	}
	r.s.SetFaults(durabletest.Faults{FailUpload: true})
	r.put("t/a", "5")
	r.clk.Add(time.Hour)
	if rep, _ = r.h.Tick(ctx); rep.Errors["t/a"] == nil {
		t.Fatalf("failing write-back not reported: %+v", rep)
	}
}

func TestTickEvictsIdleAndOverBudget(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.Policy = vactor.Policy{IdleAfter: time.Minute, MaxActive: 2} })
	for _, k := range []string{"t/a", "t/b", "t/c"} {
		r.put(k, k)
		r.clk.Add(time.Second)
	}
	rep, _ := r.h.Tick(ctx)
	if len(rep.Plan.Passivate) != 1 || rep.Plan.Passivate[0] != (vactor.Eviction{Key: "t/a", Reason: vactor.ReasonBudget}) {
		t.Fatalf("plan %+v; want the LRU t/a evicted for budget", rep.Plan)
	}
	if r.h.Active() != 2 || r.published("t/a") != "t/a" {
		t.Fatal("budget eviction did not write back and drop t/a")
	}
	r.clk.Add(time.Minute)
	if rep, _ = r.h.Tick(ctx); len(rep.Plan.Passivate) != 2 || r.h.Active() != 0 {
		t.Fatalf("idle plan %+v, active %d", rep.Plan, r.h.Active())
	}
}

func TestRunMaintenanceStopsAndReports(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) { o.Policy = vactor.Policy{WriteBackCommits: 1} })
	r.put("t/a", "v1")
	c, cancel := context.WithCancel(ctx)
	got := make(chan vactor.TickReport, 64)
	done := make(chan error, 1)
	go func() {
		done <- r.h.RunMaintenance(c, time.Millisecond, func(tr vactor.TickReport) {
			select {
			case got <- tr:
			default:
			}
		})
	}()
	deadline := time.After(5 * time.Second)
	for r.published("t/a") != "v1" {
		select {
		case <-got:
		case <-deadline:
			t.Fatal("maintenance never wrote back")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunMaintenance = %v", err)
	}
	if err := r.h.RunMaintenance(ctx, 0, nil); err == nil {
		t.Fatal("interval 0 accepted")
	}
	r.h.Close()
	r.ended = true
	if err := r.h.RunMaintenance(ctx, time.Millisecond, nil); !errors.Is(err, vactor.ErrClosed) {
		t.Fatalf("RunMaintenance after Close = %v", err)
	}
}

func TestNonDurableOpenerIsRejectedInDurableMode(t *testing.T) {
	s := durabletest.NewStore()
	h, err := vactor.NewHost(nil, plainOpener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: s, CreateMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.Get(ctx, "t/a"); err == nil || !strings.Contains(err.Error(), "cannot be written back") {
		t.Fatalf("Get with a non-durable opener: %v", err)
	}
}

type plainOpener struct{}

func (plainOpener) Open(string) (vactor.DB, error) { return plainDB{}, nil }

type plainDB struct{}

func (plainDB) Query(context.Context, string) error { return nil }
func (plainDB) Close()                              {}

// A broken Store adapter trips the checked wrapper's assertion instead of corrupting state later.
func TestBrokenStoreTripsAssertion(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out")
	}
	h, err := vactor.NewHost(nil, &vactortest.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"),
		Store: noVersionStore{durabletest.NewStore()}, CreateMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		p := recover()
		if p == nil || !strings.Contains(p.(string), "assertion failed: PublishManifest") {
			t.Fatalf("recovered %v, want the PublishManifest assertion", p)
		}
	}()
	res, err := h.Get(ctx, "t/a")
	if err != nil {
		t.Fatal(err)
	}
	res.DB.Query(ctx, vactortest.Put("x"))
	h.WriteBack(ctx, "t/a")
}

type noVersionStore struct{ *durabletest.Store }

func (s noVersionStore) PublishManifest(c context.Context, m durable.Manifest, prev durable.Version) (durable.Version, error) {
	s.Store.PublishManifest(c, m, prev)
	return durable.NoVersion, nil
}

func TestOwnershipHooks(t *testing.T) {
	refuse := errors.New("not the owner")
	var guarded []string
	r := newDurable(t, func(o *vactor.Options) {
		o.CheckActivation = func(key string, epoch uint64, m durable.Manifest) error {
			if key == "t/no" {
				return refuse
			}
			return nil
		}
		o.Guard = func(c context.Context, key string, epoch uint64, do func(context.Context) error) error {
			guarded = append(guarded, fmt.Sprint(key, "@", epoch))
			return do(c)
		}
		o.Policy = vactor.Policy{WriteBackCommits: 1}
	})
	if _, err := r.h.Get(ctx, "t/no"); !errors.Is(err, refuse) {
		t.Fatalf("CheckActivation refusal: %v", err)
	}
	if _, err := r.h.GetAt(ctx, "t/a", 2); err != nil {
		t.Fatal(err)
	}
	r.put("t/a", "v1")
	if rep, _ := r.h.Tick(ctx); len(rep.WroteBack) != 1 || len(guarded) != 1 || guarded[0] != "t/a@2" {
		t.Fatalf("Tick write-back not guarded: %+v, %v", rep, guarded)
	}
	res, err := r.h.WriteBackAt(ctx, "t/a", 3)
	if err != nil || res.Wrote || res.Generation != 1 {
		t.Fatalf("clean WriteBackAt = %+v, %v; want the current generation 1", res, err)
	}
	r.put("t/a", "v2")
	if res, err := r.h.WriteBackAt(ctx, "t/a", 3); err != nil || res.Generation != 2 {
		t.Fatalf("WriteBackAt = %+v, %v", res, err)
	}
	if m, _ := r.s.Manifest("t/a"); m.Epoch != 3 {
		t.Fatalf("published at epoch %d, want 3", m.Epoch)
	}
}

// A panic in a Tick worker (here: the Guard) reaches Tick's caller with the worker's stack, after the other
// workers stopped (Cox-Buday: panics do not cross goroutines on their own).
func TestTickForwardsWorkerPanic(t *testing.T) {
	r := newDurable(t, func(o *vactor.Options) {
		o.Policy = vactor.Policy{WriteBackCommits: 1}
		o.Guard = func(context.Context, string, uint64, func(context.Context) error) error { panic("guard bug") }
	})
	r.put("t/a", "v1")
	r.put("t/b", "v1")
	defer func() {
		p, ok := recover().(*panicx.Panic)
		if !ok || p.Value != "guard bug" || !strings.Contains(p.Stack, "TestTickForwardsWorkerPanic") {
			t.Fatalf("recovered %#v, want the guard's panic with its stack", p)
		}
	}()
	r.h.Tick(ctx)
}

func TestPassivateWithALeftoverWALAndAFailingSnapshotStaysWarm(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	os.WriteFile(r.h.LocalPath("t/a")+"-wal", []byte("frames"), 0o644)
	r.o.SetFailSnapshot(true)
	err := r.h.Passivate(ctx, "t/a")
	mustModule(t, err, "WAL left after close and the snapshot failed")
	if r.h.Active() != 1 || r.local("t/a") != "v1" {
		t.Fatal("not warm after the failed passivation")
	}
	r.o.SetFailSnapshot(false)
	r.o.Set(true, false) // reopen for the WAL snapshot fails: the entry is stranded, the file kept
	if err := r.h.Passivate(ctx, "t/a"); err == nil {
		t.Fatal("Passivate with a failing reopen succeeded")
	}
	r.o.Set(false, false)
	os.Remove(r.h.LocalPath("t/a") + "-wal")
	if err := r.h.Passivate(ctx, "t/a"); err != nil || r.published("t/a") != "v1" {
		t.Fatalf("final Passivate: %v, published %q", err, r.published("t/a"))
	}
}

// RI-14 / OS-02: a warm Stamp whose publish applied but was reported ErrStale (a re-send of a lost reply
// finds the version moved) or lost must re-read and succeed, not mark the key superseded forever.
func TestWarmStampLostReplyIsNotSuperseded(t *testing.T) {
	for _, f := range []durabletest.Faults{{ApplyThenStale: true}, {LosePublishReply: true}} {
		t.Run(fmt.Sprintf("%+v", f), func(t *testing.T) {
			r := newDurable(t, nil)
			m := r.s.Seed("t/a", 1, []byte("v1"))
			if _, err := r.h.GetAt(ctx, "t/a", 1); err != nil {
				t.Fatal(err)
			}
			r.s.SetFaults(f)
			got, err := r.h.Stamp(ctx, "t/a", 2, m.Generation)
			r.s.SetFaults(durabletest.Faults{})
			if err != nil || got.Epoch != 2 {
				t.Fatalf("warm Stamp = %+v, %v; the re-read shows our stamp applied", got, err)
			}
			r.put("t/a", "v2")
			if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
				t.Fatalf("write-back after a resolved stamp: %v", err)
			}
			if st, _ := r.s.Manifest("t/a"); st.Epoch != 2 || r.published("t/a") != "v2" {
				t.Fatalf("manifest %+v published %q", st, r.published("t/a"))
			}
		})
	}
}

// A stamp that fails without another writer showing up (the re-read finds the manifest we last saw) must
// not mark the key stale either: only a re-read showing another writer does (object-storage guide R4).
func TestWarmStampFailureWithoutAnotherWriterIsNotStale(t *testing.T) {
	r := newDurable(t, nil)
	m := r.s.Seed("t/a", 0, []byte("v1"))
	r.get("t/a")
	if _, err := r.h.Stamp(ctx, "t/a", 2, m.Generation+1); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("stamp at a wrong generation: %v", err)
	}
	r.s.SetFaults(durabletest.Faults{FailPublish: true})
	if _, err := r.h.Stamp(ctx, "t/a", 2, m.Generation); err == nil {
		t.Fatal("stamp with a failing publish succeeded")
	}
	r.s.SetFaults(durabletest.Faults{})
	r.put("t/a", "v2")
	if _, err := r.h.WriteBack(ctx, "t/a"); err != nil {
		t.Fatalf("write-back after failed stamps: %v", err)
	}
}

// OS-02: an upload whose reply was lost is resolved by HEADing the object and comparing its sha256: it is
// adopted, not uploaded again under the next generation (which would cost a full upload and an orphan).
func TestLostUploadReplyIsAdoptedByHash(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	r.s.SetFaults(durabletest.Faults{LoseUploadReply: true})
	res, err := r.h.WriteBack(ctx, "t/a")
	r.s.SetFaults(durabletest.Faults{})
	if err != nil || !res.Wrote || res.Generation != 1 || r.published("t/a") != "v1" {
		t.Fatalf("WriteBack = %+v, %v; published %q", res, err, r.published("t/a"))
	}
	if puts, _, _ := r.s.Counts(); puts != 1 {
		t.Fatalf("%d uploads, want 1", puts)
	}
}

// An existing object at the next generation with other bytes is someone else's (or an orphan): skipped.
func TestExistingGenerationWithOtherBytesIsSkipped(t *testing.T) {
	r := newDurable(t, nil)
	r.put("t/a", "v1")
	path := filepath.Join(t.TempDir(), "x")
	os.WriteFile(path, []byte("other"), 0o644)
	if err := r.s.PutGeneration(ctx, durable.NextManifest("t/a", 1, 0, durabletest.DigestOf([]byte("other")), time.Now()), path); err != nil {
		t.Fatal(err)
	}
	res, err := r.h.WriteBack(ctx, "t/a")
	if err != nil || res.Generation != 2 || r.published("t/a") != "v1" {
		t.Fatalf("WriteBack = %+v, %v", res, err)
	}
}
