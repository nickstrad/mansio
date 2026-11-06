package front_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
)

func TestFakeExecutorContract(t *testing.T) {
	fronttest.RunExecutorSuite(t, func(t *testing.T) front.Executor { return fronttest.NewExecutor(fronttest.SuiteDB) })
}

// kvActor seeds a database with fronttest.KVSchema and KVRows and starts its actor.
func kvActor(t testing.TB, cfg actor.Config) *actor.Actor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	err := actor.Seed(path, cfg, fronttest.KVSchema, func(exec func(string, ...any) error) error {
		for _, r := range fronttest.KVRows {
			if err := exec(fronttest.PutSQL, r...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a := actor.New(0, path, cfg)
	if err := a.Start(true); err != nil {
		t.Fatal(err)
	}
	return a
}

func realExecutor(t *testing.T, cfg actor.Config) (front.ActorExecutor, *front.StaticHost) {
	h := front.NewStaticHost(map[string]*actor.Actor{fronttest.SuiteDB: kvActor(t, cfg)})
	t.Cleanup(h.Close)
	return front.ActorExecutor{Host: h, Grace: time.Second}, h
}

func TestActorExecutorContract(t *testing.T) {
	fronttest.RunExecutorSuite(t, func(t *testing.T) front.Executor { e, _ := realExecutor(t, actor.Config{}); return e })
}

func TestActorExecutorContractWithReadPool(t *testing.T) {
	fronttest.RunExecutorSuite(t, func(t *testing.T) front.Executor {
		e, _ := realExecutor(t, actor.Config{ReadConns: 2})
		return e
	})
}

func call(sql string, kind front.Kind, args ...any) front.Call {
	return front.Call{DB: fronttest.SuiteDB, Kind: kind, SQL: sql, Args: args, Deadline: time.Now().Add(5 * time.Second),
		StmtTimeout: time.Second, MaxRows: 100}
}

// Failure paths of the actor adapter beyond the suite.
func TestActorExecutorFailurePaths(t *testing.T) {
	ctx := context.Background()
	t.Run("closed host sheds", func(t *testing.T) {
		e, h := realExecutor(t, actor.Config{})
		h.Close()
		h.Close() // idempotent
		if _, err := e.Exec(ctx, call(fronttest.GetSQL, front.KindRead, int64(1))); !errors.Is(err, front.ErrShed) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("closed actor sheds", func(t *testing.T) {
		a := kvActor(t, actor.Config{})
		a.Close()
		e := front.ActorExecutor{Host: front.NewStaticHost(map[string]*actor.Actor{fronttest.SuiteDB: a})}
		if _, err := e.Exec(ctx, call(fronttest.GetSQL, front.KindRead, int64(1))); front.Classify(err) != front.Shed {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("full mailbox sheds", func(t *testing.T) {
		e, _ := realExecutor(t, actor.Config{MailboxSize: 1, MaxBatch: 1})
		slow := call(fronttest.SlowSQL, front.KindRead)
		slow.StmtTimeout = 300 * time.Millisecond
		errc := make(chan error, 8)
		for range 8 {
			go func() { _, err := e.Exec(ctx, slow); errc <- err }()
		}
		var shed int
		for range 8 {
			if errors.Is(<-errc, front.ErrShed) {
				shed++
			}
		}
		if shed == 0 {
			t.Fatal("8 slow calls on a 1-slot mailbox: none shed")
		}
	})
	t.Run("aborted batch-mate", func(t *testing.T) {
		e, _ := realExecutor(t, actor.Config{MaxBatch: 64})
		slowW := call(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1000000000) INSERT INTO kv (k, v) SELECT 1000 + COUNT(*), 'x' FROM c`, front.KindWrite)
		slowW.StmtTimeout = 100 * time.Millisecond
		// Hold the actor with a slow read so the next two writes land in one batch.
		block := call(fronttest.SlowSQL, front.KindRead)
		block.StmtTimeout = 100 * time.Millisecond
		go e.Exec(ctx, block)
		time.Sleep(20 * time.Millisecond)
		errc := make(chan error, 2)
		go func() { _, err := e.Exec(ctx, slowW); errc <- err }()
		time.Sleep(5 * time.Millisecond)
		go func() { _, err := e.Exec(ctx, call(fronttest.PutSQL, front.KindWrite, int64(50), "x")); errc <- err }()
		got := []error{<-errc, <-errc}
		var timedOut, aborted bool
		for _, err := range got {
			timedOut = timedOut || errors.Is(err, front.ErrStmtTimeout)
			aborted = aborted || errors.Is(err, front.ErrAborted)
		}
		if !timedOut || !aborted {
			t.Skipf("the two writes did not share a batch this time: %v", got) // timing-dependent; covered in actor tests
		}
	})
	t.Run("no reply before deadline + grace", func(t *testing.T) {
		e, _ := realExecutor(t, actor.Config{})
		e.Grace = time.Millisecond
		c := call(fronttest.SlowSQL, front.KindWrite)
		c.Deadline = time.Now().Add(20 * time.Millisecond)
		c.StmtTimeout = 500 * time.Millisecond // longer than the deadline: a caller bug the grace still bounds
		t0 := time.Now()
		if _, err := e.Exec(ctx, c); !errors.Is(err, front.ErrNoReply) || time.Since(t0) > 400*time.Millisecond {
			t.Fatalf("err %v after %v", err, time.Since(t0))
		}
	})
	t.Run("client gone does not abandon a submitted write", func(t *testing.T) {
		e, _ := realExecutor(t, actor.Config{})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := e.Exec(cctx, call(fronttest.PutSQL, front.KindWrite, int64(60), "x")); err != nil {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("unexpected SQLite error is internal and wrapped", func(t *testing.T) {
		e, _ := realExecutor(t, actor.Config{})
		// An arg type the driver cannot bind fails outside SQLite: not a known statement error, so internal.
		_, err := e.Exec(ctx, call(fronttest.GetSQL, front.KindRead, struct{}{}))
		if front.Classify(err) != front.Internal {
			t.Fatalf("err %v classified %s", err, front.Classify(err))
		}
		if outer, ok := errs.Outermost(err); !ok || outer.Module != "front" {
			t.Fatalf("not wrapped by front: %T %v", err, err)
		}
	})
}

func TestStaticHost(t *testing.T) {
	h := front.NewStaticHost(map[string]*actor.Actor{})
	if _, err := h.Lookup(context.Background(), "x"); !errors.Is(err, front.ErrUnknownDB) {
		t.Fatal(err)
	}
}
