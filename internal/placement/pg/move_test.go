package pg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/ts-db-go/internal/faultnet"
	"github.com/nickstrad/ts-db-go/internal/owner"
	"github.com/nickstrad/ts-db-go/internal/owner/ownertest"
)

// moveProc is one simulated agent process: its ownership table over the real authority, and fakes for its
// local disk and object storage (the owner package's harness, trimmed to one move).
type moveProc struct {
	tab *owner.Table
	wb  *ownertest.WriteBack
}

// agent is what the Mover calls; a successful Claim that activates the key fetches it (the host's job).
type agent struct{ p moveProc }

func (a agent) Fence(ctx context.Context, key string) (owner.Ack, error) {
	return a.p.tab.Fence(ctx, key)
}

func (a agent) Claim(ctx context.Context, key string) (owner.Local, error) {
	before := a.p.tab.State(key)
	l, err := a.p.tab.Claim(ctx, key)
	if err == nil && (before.State != owner.Active || before.Epoch != l.Epoch) {
		if err := a.p.tab.CheckActivation(ctx, key, l.Epoch, a.p.wb.Fetch(key)); err != nil {
			return owner.Local{}, err
		}
	}
	return l, err
}

// flipHook runs before on the Mover's flip CAS (MOVING → ACTIVE), once.
type flipHook struct {
	owner.Placement
	once   sync.Once
	before func(key string)
}

func (h *flipHook) CAS(ctx context.Context, key string, expect, next owner.Place) error {
	if expect.State == owner.PlaceMoving && next.State == owner.PlaceActive {
		h.once.Do(func() { h.before(key) })
	}
	return h.Placement.CAS(ctx, key, expect, next)
}

// TestMoveCrashPoints, "flip applied but its reply was lost", with the real adapter (PG-03): the flip's
// statement reaches Postgres and commits, but its reply never reaches the Mover. The Mover sees a
// non-sentinel error (not ErrConflict), and calling Move again resumes from Get: the target claims at the
// new epoch, nothing acknowledged is lost, the source is fenced, and exactly one moves row exists.
//
// How the reply is lost: a second session holds the row lock, so the flip waits inside Postgres after it was
// sent (seen in pg_stat_activity); then the proxy freezes the Mover's connection and the lock is released.
// The flip commits; its reply is dropped by the frozen link; the call fails at CallTimeout.
func TestMoveCrashPoints(t *testing.T) {
	t.Run("flip applied but its reply was lost", func(t *testing.T) {
		direct, schema := newPlacement(t, nil)
		moverPl, px := proxied(t, schema, func(c *Config) { c.AppName = "test/mover" }) // default 1 s lock_timeout: the poll below needs it
		objs := ownertest.NewObjects()
		procs := map[string]moveProc{}
		for _, n := range []string{"a", "b"} {
			wb := &ownertest.WriteBack{Objects: objs, Disk: ownertest.NewDisk()}
			tab, err := owner.Open(ctx, owner.Config{Self: n, Store: ownertest.NewMemStore(), Placement: direct, WriteBack: wb})
			must(t, err)
			procs[n] = moveProc{tab: tab, wb: wb}
		}
		// Placed on a at epoch 1, three acknowledged writes.
		must(t, direct.CAS(ctx, "k", owner.Place{}, owner.Place{Owner: "a", Epoch: 1, State: owner.PlaceActive}))
		_, err := procs["a"].tab.Claim(ctx, "k")
		must(t, err)
		procs["a"].wb.Fetch("k")
		var acked []string
		for i := range 3 {
			id := fmt.Sprintf("w%d", i)
			must(t, procs["a"].tab.Do("k", 1, func() error { procs["a"].wb.Disk.Append("k", id); return nil }))
			acked = append(acked, id)
		}

		admin, poller := adminConn(t, schema), adminConn(t, "")
		done := make(chan error, 1)
		hook := &flipHook{Placement: moverPl, before: func(key string) {
			tx, err := admin.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			if _, err := tx.Exec(ctx, "select 1 from placement where database_id = $1 for update", key); err != nil {
				done <- err
				return
			}
			go func() { done <- loseTheFlipReply(tx.Commit, px, poller) }()
		}}
		mover := &owner.Mover{Placement: hook, Agent: func(n string) owner.Agent { return agent{procs[n]} }}
		_, err = mover.Move(ctx, "k", "b")
		if err == nil {
			t.Fatal("the lost reply did not stop the move")
		}
		if e := <-done; e != nil {
			t.Fatalf("injecting the lost reply: %v", e)
		}
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, owner.ErrConflict) {
			t.Fatalf("Move with the flip's reply lost = %v; want ErrUnavailable, not ErrConflict", err)
		}
		// The flip applied although the Mover could not know.
		row, err := direct.Get(ctx, "k")
		must(t, err)
		if row.Owner != "b" || row.Epoch != 2 || row.State != owner.PlaceActive {
			t.Fatalf("row after the lost reply = %+v; the flip should have committed", row)
		}
		// While stuck, b has not claimed: only the fenced source knows the key, and it admits nothing.
		if _, err := procs["a"].tab.Admit("k", 1); err != owner.ErrFenced {
			t.Fatalf("source Admit while stuck = %v, want ErrFenced", err)
		}

		// Resume.
		px.Heal()
		l, err := mover.Move(ctx, "k", "b")
		must(t, err)
		if l.Epoch != 2 {
			t.Fatalf("resumed Move = %s, want epoch 2", l)
		}
		if got := procs["b"].tab.State("k"); got.State != owner.Active || got.Epoch != 2 {
			t.Fatalf("target = %s", got)
		}
		if _, e, _, _, _ := objs.Read("k"); e != 2 {
			t.Fatalf("storage epoch = %d, want the target's stamp 2", e)
		}
		if got := procs["b"].wb.Disk.Contents("k"); !slices.Equal(got, acked) {
			t.Fatalf("target starts from %v, want exactly the acknowledged %v", got, acked)
		}
		if _, err := procs["b"].tab.Admit("k", 1); err != owner.ErrWrongOwner {
			t.Fatalf("target Admit at the old epoch = %v, want ErrWrongOwner", err)
		}
		var moves int
		must(t, admin.QueryRow(ctx, "select count(*) from moves where database_id = 'k' and from_epoch = 1 and ack_generation = $1", int64(row.Gen)).Scan(&moves))
		if moves != 1 {
			t.Fatalf("moves rows for the flip = %d, want exactly 1", moves)
		}
		if v, err := direct.Invariants(ctx, 10); err != nil || len(v) != 0 {
			t.Fatalf("Invariants = %+v, %v", v, err)
		}
	})
}

// loseTheFlipReply waits until the Mover's flip is blocked on the row lock inside Postgres (so its
// statement was sent), freezes the Mover's live connection, then releases the lock so the flip commits
// into a link that drops the reply.
func loseTheFlipReply(commit func(context.Context) error, px *faultnet.Proxy, poller *pgx.Conn) error {
	pollCtx, cancel := context.WithTimeout(ctx, 900*time.Millisecond) // under lock_timeout (1 s)
	defer cancel()
	for {
		var waiting bool
		if err := poller.QueryRow(pollCtx, `select exists (select 1 from pg_stat_activity
		     where application_name = 'test/mover' and wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
			return fmt.Errorf("polling for the blocked flip: %w", err)
		}
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	px.Set(faultnet.Frozen(), 0)
	px.Heal()
	return commit(ctx)
}
