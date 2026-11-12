package agentglue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/front/fronttest"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/actordb"
)

// glueRig is the durable-mode wiring of newDurable over fakes: the in-memory durable store, the fake
// authority and an in-memory owner store, with the real host, table and glue between them.
type glueRig struct {
	store *durabletest.Store
	place *ownertest.Placement
	host  *vactor.Host
	tab   *owner.Table
	exec  front.Executor
	adm   front.Admitter
}

func newGlueRig(t *testing.T, policy vactor.Policy) *glueRig {
	t.Helper()
	r := &glueRig{store: durabletest.NewStore(), place: ownertest.NewPlacement()}
	var err error
	r.host, err = vactor.NewHost(nil, actordb.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "cache"), Store: r.store,
		Policy: policy, CheckActivation: ActivationCheck(nil, func() *owner.Table { return r.tab }),
		Guard: PublishGuard(func() *owner.Table { return r.tab }, func() *vactor.Host { return r.host })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.host.Close() })
	r.tab, err = owner.Open(context.Background(), owner.Config{Self: "vm-1", Store: ownertest.NewMemStore(), Placement: r.place,
		WriteBack: WriteBack{Host: r.host, Store: r.store}})
	if err != nil {
		t.Fatal(err)
	}
	r.exec, r.adm = Executor{Host: r.host, Grace: time.Second}, Admitter{r.tab}
	return r
}

// claim creates key's row (epoch 1) as the control plane would, then claims it here.
func (r *glueRig) claim(t *testing.T, key string) owner.Local {
	t.Helper()
	r.place.Create(key, owner.Place{Owner: "vm-1", Epoch: 1, State: owner.PlaceActive})
	l, err := r.tab.Claim(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// exec runs one statement through admission and the executor, as the front does.
func (r *glueRig) run(t *testing.T, key string, epoch uint64, kind front.Kind, sql string, args ...any) (front.Reply, error) {
	t.Helper()
	release, err := r.adm.Admit(key, epoch)
	if err != nil {
		return front.Reply{}, err
	}
	defer release()
	return r.exec.Exec(context.Background(), front.Call{DB: key, Epoch: epoch, Kind: kind, SQL: sql, Args: args,
		Deadline: time.Now().Add(5 * time.Second), StmtTimeout: time.Second, MaxRows: 100})
}

// seedFile returns the bytes of a kv database holding fronttest.KVRows.
func seedFile(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.db")
	err := actor.Seed(path, actor.Config{}, fronttest.KVSchema, func(exec func(string, ...any) error) error {
		for _, row := range fronttest.KVRows {
			if err := exec(fronttest.PutSQL, row...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// epochOne runs every call at epoch 1 (the suite's calls carry none; the seeded manifest is at 1).
type epochOne struct{ front.Executor }

func (e epochOne) Exec(ctx context.Context, c front.Call) (front.Reply, error) {
	c.Epoch = 1
	return e.Executor.Exec(ctx, c)
}

// The executor over the virtual-actor host passes the shared executor contract. Activation runs without
// an ownership check here (the suite has no admission); the next tests cover the check.
func TestHostExecutorContract(t *testing.T) {
	data := seedFile(t)
	fronttest.RunExecutorSuite(t, func(t *testing.T) front.Executor {
		st := durabletest.NewStore()
		st.Seed(fronttest.SuiteDB, 1, data)
		h, err := vactor.NewHost(nil, actordb.Opener{}, vactor.Options{CacheDir: filepath.Join(t.TempDir(), "c"), Store: st})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		return epochOne{Executor{Host: h, Grace: time.Second}}
	})
}

// Flow B end to end over the glue: claim stamps a generation-0 manifest, writes are admitted at the claimed
// epoch only, the fence publishes them and drops the warm copy, and the key then admits nothing.
func TestClaimWriteFence(t *testing.T) {
	r := newGlueRig(t, vactor.Policy{})
	if l := r.claim(t, "db-1"); l.State != owner.Active || l.Epoch != 1 || l.Gen != 0 {
		t.Fatalf("claim: %s", l)
	}
	if m, ok := r.store.Manifest("db-1"); !ok || m.Epoch != 1 || m.Generation != 0 {
		t.Fatalf("claim did not stamp: %+v %v", m, ok)
	}
	if _, err := r.run(t, "db-1", 1, front.KindWrite, fronttest.KVSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(t, "db-1", 1, front.KindWrite, fronttest.PutSQL, 1, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(t, "db-1", 2, front.KindRead, fronttest.GetSQL, 1); !errors.Is(err, front.ErrWrongOwner) {
		t.Fatalf("epoch 2 = %v, want wrong owner", err)
	}
	ack, err := r.tab.Fence(context.Background(), "db-1")
	if err != nil || ack.Epoch != 1 || ack.Gen < 1 {
		t.Fatalf("fence: %+v %v", ack, err)
	}
	if lost := r.host.Discard("db-1"); lost {
		t.Fatal("the fence left unpublished writes")
	}
	if m, _ := r.store.Manifest("db-1"); m.Generation != uint64(ack.Gen) {
		t.Fatalf("manifest at generation %d, ack says %d", m.Generation, ack.Gen)
	}
	if _, err := r.run(t, "db-1", 1, front.KindRead, fronttest.GetSQL, 1); !errors.Is(err, front.ErrFenced) {
		t.Fatalf("after the fence = %v, want fenced", err)
	}
	// Fencing again is idempotent; a fence of a key that is cold here returns the manifest's generation.
	if again, err := r.tab.Fence(context.Background(), "db-1"); err != nil || again != ack {
		t.Fatalf("second fence: %+v %v", again, err)
	}
}

// A key passivated before its fence: the fence finds nothing warm and acknowledges the manifest's generation.
func TestFenceOfColdKeyAcksManifestGeneration(t *testing.T) {
	r := newGlueRig(t, vactor.Policy{})
	r.claim(t, "db-1")
	if _, err := r.run(t, "db-1", 1, front.KindWrite, fronttest.KVSchema); err != nil {
		t.Fatal(err)
	}
	if err := r.host.Passivate(context.Background(), "db-1"); err != nil {
		t.Fatal(err)
	}
	m, _ := r.store.Manifest("db-1")
	ack, err := r.tab.Fence(context.Background(), "db-1")
	if err != nil || uint64(ack.Gen) != m.Generation || m.Generation == 0 {
		t.Fatalf("fence of a cold key: %+v %v (manifest %d)", ack, err, m.Generation)
	}
}

// A cold key whose manifest a higher epoch stamped: the fence must not acknowledge; it is superseded.
func TestFenceOfColdKeySupersededByStorage(t *testing.T) {
	r := newGlueRig(t, vactor.Policy{})
	r.claim(t, "db-1")
	r.store.Bump("db-1", 2)
	if _, err := r.tab.Fence(context.Background(), "db-1"); !errors.Is(err, owner.ErrSuperseded) {
		t.Fatalf("fence = %v, want superseded", err)
	}
}

// OS R4 through the glue: another writer stamps a higher epoch; the next maintenance write-back finds it,
// the key is fenced durably as superseded, its warm copy dropped without a write-back, and requests answer
// fenced; a cold activation at the old epoch is refused before any fetch.
func TestStaleWriteBackSupersedes(t *testing.T) {
	r := newGlueRig(t, vactor.Policy{WriteBackCommits: 1})
	r.claim(t, "db-1")
	if _, err := r.run(t, "db-1", 1, front.KindWrite, fronttest.KVSchema); err != nil {
		t.Fatal(err)
	}
	r.store.Bump("db-1", 2)
	rep, err := r.host.Tick(context.Background())
	if err != nil || !errors.Is(rep.Errors["db-1"], owner.ErrSuperseded) {
		t.Fatalf("tick: %+v %v", rep.Errors, err)
	}
	if l := r.tab.State("db-1"); l.State != owner.Fenced || !l.Superseded {
		t.Fatalf("state %s", l)
	}
	if g := r.host.Gauges(); g.Active != 0 {
		t.Fatalf("superseded copy still warm: %+v", g)
	}
	if _, err := r.run(t, "db-1", 1, front.KindRead, "SELECT 1"); !errors.Is(err, front.ErrFenced) {
		t.Fatalf("request after supersede = %v", err)
	}
}

// CheckActivation runs before any fetch: a key not held here never downloads (only a claim activates).
func TestActivationOnlyForClaimedKeys(t *testing.T) {
	r := newGlueRig(t, vactor.Policy{})
	r.store.Seed("db-x", 1, seedFile(t))
	_, err := r.exec.Exec(context.Background(), front.Call{DB: "db-x", Epoch: 1, Kind: front.KindRead, SQL: "SELECT 1",
		Deadline: time.Now().Add(time.Second), StmtTimeout: time.Second, MaxRows: 1})
	if !errors.Is(err, front.ErrWrongOwner) {
		t.Fatalf("unclaimed key = %v, want wrong owner", err)
	}
	if _, _, fetches := r.store.Counts(); fetches != 0 {
		t.Fatalf("%d fetches for a key not held here", fetches)
	}
}

func TestAdmitterMapping(t *testing.T) {
	r := newGlueRig(t, vactor.Policy{})
	r.claim(t, "db-1")
	release, err := r.adm.Admit("db-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	release()
	for _, tc := range []struct {
		db    string
		epoch uint64
		want  error
	}{{"db-1", 2, front.ErrWrongOwner}, {"db-1", 0, front.ErrWrongOwner}, {"nope", 1, front.ErrWrongOwner}} {
		if _, err := r.adm.Admit(tc.db, tc.epoch); !errors.Is(err, tc.want) {
			t.Errorf("Admit(%s, %d) = %v, want %v", tc.db, tc.epoch, err, tc.want)
		}
	}
	if _, err := r.tab.Fence(context.Background(), "db-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.adm.Admit("db-1", 1); !errors.Is(err, front.ErrFenced) {
		t.Fatalf("fenced = %v", err)
	}
	// A key loaded closed after a restart is unavailable until reconciled (depAdmitter maps it).
	st := ownertest.NewMemStore()
	st.Put(context.Background(), owner.Record{Key: "db-2", State: owner.Active, Epoch: 1})
	tab, err := owner.Open(context.Background(), owner.Config{Self: "vm-1", Store: st, Placement: r.place, WriteBack: WriteBack{r.host, r.store}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (DepAdmitter{Admitter{tab}}).Admit("db-2", 1); !errors.Is(err, front.ErrUnavailable) {
		t.Fatalf("closed = %v", err)
	}
}

func TestActivationErrorMapping(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		ctx  context.Context
		err  error
		want error
	}{
		{context.Background(), owner.ErrSuperseded, front.ErrFenced},
		{context.Background(), owner.ErrMoving, front.ErrFenced},
		{context.Background(), owner.ErrWrongOwner, front.ErrWrongOwner},
		{context.Background(), durable.ErrStale, front.ErrWrongOwner},
		{context.Background(), vactor.ErrNotFound, front.ErrUnknownDB},
		{context.Background(), vactor.ErrClosed, front.ErrShed},
		{cancelled, fmt.Errorf("x: %w", context.Canceled), front.ErrExpired},
		{context.Background(), vactor.ErrBusy, vactor.ErrBusy},                 // depExecutor's
		{context.Background(), durable.ErrUnavailable, durable.ErrUnavailable}, // depExecutor's
	} {
		got := ActivationError(tc.ctx, fmt.Errorf("wrapped: %w", tc.err))
		if !errors.Is(got, tc.want) || !errors.Is(got, tc.err) {
			t.Errorf("%v → %v, want %v (chain kept)", tc.err, got, tc.want)
		}
	}
	if got := AsUnavailable(ActivationError(context.Background(), durable.ErrCorrupt)); front.Classify(got) != front.Internal {
		t.Errorf("a corrupt generation answers %s, want internal", front.Classify(got))
	}
}
