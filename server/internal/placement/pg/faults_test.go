package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/faultnet"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// Failures below the interface (Release It H4): a held row lock, a black hole, a half-open connection, a
// refused connection. None may hang past its timeout or look like an answer (ErrConflict, ErrNotFound).

const slack = 750 * time.Millisecond // scheduling and connection teardown under -race

func wantUnavailable(t *testing.T, what string, err error, took, limit time.Duration) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrUnavailable) || errors.Is(err, owner.ErrConflict) || errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("%s = %v; want ErrUnavailable, neither ErrConflict nor ErrNotFound", what, err)
	}
	if took > limit {
		t.Fatalf("%s took %v, want ≤ %v", what, took, limit)
	}
}

func timed(f func() error) (error, time.Duration) {
	start := time.Now()
	err := f()
	return err, time.Since(start)
}

// proxied opens an adapter on schema through a faultnet proxy in front of MANSIO_PG_URL. It does not migrate.
func proxied(t *testing.T, schema string, mod func(*Config)) (*Placement, *faultnet.Proxy) {
	t.Helper()
	u, addr := pgAddr(t)
	px, err := faultnet.New(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { px.Close() })
	u.Host = px.Addr()
	return open(t, u.String(), schema, mod), px
}

// short shrinks the timeouts so fault tests wait less; the assertions scale with p.cfg.
func short(c *Config) {
	c.StatementTimeout, c.LockTimeout, c.CallTimeout = 300*time.Millisecond, 200*time.Millisecond, 500*time.Millisecond
}

var a1 = owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceActive}
var a1m = owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceMoving}

// A session holding the row in an open transaction makes CAS fail within lock_timeout, as a non-sentinel
// error; reads are not blocked (MVCC), and the CAS works once the lock is gone.
func TestLockHeldFailsWithinLockTimeout(t *testing.T) {
	p, schema := newPlacement(t, short)
	must(t, p.CAS(ctx, "k", owner.Place{}, a1))
	c := adminConn(t, schema)
	tx, err := c.Begin(ctx)
	must(t, err)
	_, err = tx.Exec(ctx, "select 1 from placement where database_id = 'k' for update")
	must(t, err)
	err, took := timed(func() error { return p.CAS(ctx, "k", a1, a1m) })
	wantUnavailable(t, "CAS on a locked row", err, took, p.cfg.LockTimeout+slack)
	if took < p.cfg.LockTimeout-50*time.Millisecond {
		t.Fatalf("CAS on a locked row gave up after %v, before lock_timeout %v", took, p.cfg.LockTimeout)
	}
	if got, err := p.Get(ctx, "k"); err != nil || got != a1 {
		t.Fatalf("Get while the row is locked = %+v, %v; want the committed row at once", got, err)
	}
	must(t, tx.Rollback(ctx))
	must(t, p.CAS(ctx, "k", a1, a1m))
}

// New connections black-holed: Get and CAS fail within CallTimeout (the network hangs, so the server's
// statement_timeout cannot answer), and calls work again after the network heals.
func TestBlackholeFailsWithinCallTimeout(t *testing.T) {
	direct, schema := newPlacement(t, nil)
	must(t, direct.CAS(ctx, "k", owner.Place{}, a1))
	p, px := proxied(t, schema, short)
	px.Set(faultnet.Hole(), 0)
	limit := p.cfg.CallTimeout + slack
	err, took := timed(func() error { _, err := p.Get(ctx, "k"); return err })
	wantUnavailable(t, "Get through a black hole", err, took, limit)
	err, took = timed(func() error { return p.CAS(ctx, "k", a1, a1m) })
	wantUnavailable(t, "CAS through a black hole", err, took, limit)
	px.Heal()
	if got, err := p.Get(ctx, "k"); err != nil || got != a1 {
		t.Fatalf("Get after heal = %+v, %v", got, err)
	}
}

// A pooled connection that goes half-open (a NAT forgot the flow) fails the next call within CallTimeout,
// is dropped from the pool, and the call after that gets a fresh connection without a hang.
func TestHalfOpenRecyclesTheStaleConnection(t *testing.T) {
	direct, schema := newPlacement(t, nil)
	must(t, direct.CAS(ctx, "k", owner.Place{}, a1))
	p, px := proxied(t, schema, func(c *Config) { c.MaxConns = 1 }) // default timeouts: CallTimeout must exceed closeBudget here
	if _, err := p.Get(ctx, "k"); err != nil {                      // warm the one pooled connection
		t.Fatal(err)
	}
	px.Set(faultnet.Frozen(), 0) // freezes the live connection...
	px.Heal()                    // ...and only it
	err, took := timed(func() error { return p.CAS(ctx, "k", a1, a1m) })
	wantUnavailable(t, "CAS on a half-open connection", err, took, p.cfg.CallTimeout+slack)
	// The frozen CAS never reached the server, so the row is unchanged (the outcome was unknown to us).
	start := time.Now()
	got, err := p.Get(ctx, "k")
	if err != nil || got != a1 {
		t.Fatalf("Get after the half-open call = %+v, %v", got, err)
	}
	if d := time.Since(start); d > closeBudget+slack {
		t.Fatalf("the next call took %v: the stale connection's pool slot was not freed in time", d)
	}
	px.Set(faultnet.Frozen(), 0)
	px.Heal()
	err, took = timed(func() error { _, err := p.Get(ctx, "k"); return err })
	wantUnavailable(t, "Get on a half-open connection", err, took, p.cfg.CallTimeout+slack)
}

// The breaker counts only system failures (Release It C2): answers keep it closed; refused connections open
// it; open, it fails fast with a retry hint and sends nothing; a caller that gave up is ignored.
func TestBreakerOpensOnSystemFailuresOnly(t *testing.T) {
	_, schema := newPlacement(t, nil)
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	p, px := proxied(t, schema, func(c *Config) {
		c.Breaker = breaker.Config{MinCalls: 4, FailureRate: 0.5, Window: time.Minute, CoolDown: 10 * time.Second}
		c.Now = clock
	})
	for range 10 {
		if _, err := p.Get(ctx, "missing"); !errors.Is(err, owner.ErrNotFound) {
			t.Fatalf("Get(missing) = %v", err)
		}
		if err := p.CAS(ctx, "missing", a1, a1m); !errors.Is(err, owner.ErrConflict) {
			t.Fatalf("CAS(missing) = %v", err)
		}
	}
	if st := p.Breaker().State(now); st != breaker.Closed {
		t.Fatalf("answers opened the breaker: %s", st)
	}
	// A caller that gave up says nothing about the authority.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	for range 10 {
		if _, err := p.Get(cctx, "k"); err == nil {
			t.Fatal("Get with a done ctx succeeded")
		}
	}
	if st := p.Breaker().State(now); st != breaker.Closed {
		t.Fatalf("canceled calls opened the breaker: %s", st)
	}
	now = now.Add(2 * time.Minute) // a fresh window: the answers above count no more
	px.Set(faultnet.RefuseConn(), 0)
	p.pool.Reset() // drop the healthy pooled connections so calls dial through the fault
	for i := range 4 {
		if _, err := p.Get(ctx, "k"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Get %d with refused connections = %v", i, err)
		}
	}
	if st := p.Breaker().State(now); st != breaker.Open {
		t.Fatalf("breaker = %s after 4 refused calls, want open", st)
	}
	accepted := px.Accepted()
	err := p.CAS(ctx, "k", a1, a1m)
	var hint interface{ RetryAfter() time.Duration }
	if !errors.Is(err, breaker.ErrOpen) || !errors.Is(err, ErrUnavailable) || !errors.As(err, &hint) || hint.RetryAfter() <= 0 {
		t.Fatalf("CAS with the breaker open = %v; want ErrOpen + ErrUnavailable with a RetryAfter hint", err)
	}
	if px.Accepted() != accepted {
		t.Fatal("an open breaker still sent the call")
	}
	// After the cool-down one probe goes through; the network healed, so the breaker closes.
	px.Heal()
	now = now.Add(11 * time.Second)
	if _, err := p.Get(ctx, "missing"); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("probe = %v", err)
	}
	if st := p.Breaker().State(now); st != breaker.Closed {
		t.Fatalf("breaker = %s after a good probe, want closed", st)
	}
}
