package pg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/owner"
)

func TestLocate(t *testing.T) {
	p, _ := newPlacement(t, nil)
	if l, err := p.Locate(ctx, "acme:none"); err != nil || l != (Location{}) {
		t.Fatalf("nothing: %+v %v", l, err)
	}
	if _, err := p.CreateDatabase(ctx, "acme", "db"); err != nil {
		t.Fatal(err)
	}
	if l, err := p.Locate(ctx, "acme:db"); err != nil || l != (Location{Intent: "creating"}) {
		t.Fatalf("registered, not placed: %+v %v", l, err)
	}
	act := owner.Place{Owner: "vm-a", Epoch: 1, Gen: 0, State: owner.PlaceActive}
	if err := p.CAS(ctx, "acme:db", owner.Place{}, act); err != nil {
		t.Fatal(err)
	}
	if l, err := p.Locate(ctx, "acme:db"); err != nil || l != (Location{Intent: "creating", Found: true, Place: act, Endpoint: "vm-a:7000"}) {
		t.Fatalf("placed: %+v %v", l, err)
	}
	if err := p.CAS(ctx, "bare", owner.Place{}, act); err != nil {
		t.Fatal(err)
	}
	if l, err := p.Locate(ctx, "bare"); err != nil || l != (Location{Found: true, Place: act, Endpoint: "vm-a:7000"}) {
		t.Fatalf("placement without a registry row: %+v %v", l, err)
	}
}

func TestAPIKeyRegistry(t *testing.T) {
	p, _ := newPlacement(t, nil)
	k := APIKey{ID: "0123456789abcdef", Tenant: "acme", Principal: "app@acme", Perms: 3, Hash: [32]byte{1, 2, 3}}
	if err := p.CreateAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if got, err := p.GetAPIKey(ctx, k.ID); err != nil || got != k {
		t.Fatalf("got %+v %v", got, err)
	}
	if err := p.CreateAPIKey(ctx, k); !errors.Is(err, ErrInvariant) {
		t.Fatalf("duplicate id: %v", err)
	}
	for _, bad := range []APIKey{{ID: k.ID, Tenant: "Acme", Principal: "a", Perms: 1}, {ID: k.ID, Tenant: "acme", Principal: "", Perms: 1},
		{ID: k.ID, Tenant: "acme", Principal: "a", Perms: 0}, {ID: k.ID, Tenant: "acme", Principal: "a", Perms: 4}, {ID: "short", Tenant: "acme", Principal: "a", Perms: 1}} {
		if err := p.CreateAPIKey(ctx, bad); !errors.Is(err, owner.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	for range 2 { // idempotent
		if err := p.RevokeAPIKey(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := p.GetAPIKey(ctx, k.ID); !got.Revoked {
		t.Fatal("not revoked")
	}
	if _, err := p.GetAPIKey(ctx, "ffffffffffffffff"); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if err := p.RevokeAPIKey(ctx, "ffffffffffffffff"); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("revoke missing key: %v", err)
	}
}

// recorder collects a listener's events.
type recorder struct {
	mu     sync.Mutex
	events []string
	prefix string // only notifications for keys with this prefix (other tests share the channel)
}

func (r *recorder) hooks() ListenHooks {
	add := func(s string) { r.mu.Lock(); r.events = append(r.events, s); r.mu.Unlock() }
	return ListenHooks{
		Ready: func() { add("ready") },
		Notify: func(key string, epoch uint64) {
			if len(key) >= len(r.prefix) && key[:len(r.prefix)] == r.prefix {
				add(key[len(r.prefix):] + ":" + strconv.FormatUint(epoch, 10))
			}
		},
		State: func(key string) {
			if len(key) >= len(r.prefix) && key[:len(r.prefix)] == r.prefix {
				add("state " + key[len(r.prefix):])
			}
		},
		Lost: func(error) { add("lost") },
	}
}

func (r *recorder) wait(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		got := append([]string(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= len(want) {
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("events %v, want %v", got, want)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events %v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func uniq() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func startListener(t *testing.T, p *Placement, r *recorder) {
	t.Helper()
	lctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Listen(lctx, ListenConfig{Ping: 200 * time.Millisecond, MaxBackoff: 200 * time.Millisecond}, r.hooks())
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// PG-10: every committed change to a row notifies key:epoch, insert and delete included.
func TestListenNotifiesEveryChange(t *testing.T) {
	p, _ := newPlacement(t, nil)
	r := &recorder{prefix: "n" + uniq() + "-"}
	startListener(t, p, r)
	r.wait(t, "ready")
	key := r.prefix + "k"
	a1 := owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceActive}
	m1 := owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceMoving}
	b2 := owner.Place{Owner: "vm-b", Epoch: 2, Gen: 5, State: owner.PlaceActive}
	m2 := owner.Place{Owner: "vm-b", Epoch: 2, Gen: 5, State: owner.PlaceMoving}
	for _, step := range [][2]owner.Place{{{}, a1}, {a1, m1}, {m1, b2}, {b2, m2}} {
		if err := p.CAS(ctx, key, step[0], step[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.DropPlacement(ctx, key, m2); err != nil {
		t.Fatal(err)
	}
	r.wait(t, "ready", "k:1", "k:1", "k:2", "k:2", "k:2")
	// A rolled-back change notifies nothing.
	c := adminConn(t, p.cfg.Schema)
	tx, _ := c.Begin(ctx)
	tx.Exec(ctx, "insert into placement (database_id, owner_vm, epoch, generation, state) values ($1, 'vm-a', 7, 0, 'ACTIVE')", r.prefix+"rolled")
	tx.Rollback(ctx)
	if err := p.CAS(ctx, r.prefix+"z", owner.Place{}, owner.Place{Owner: "vm-a", Epoch: 3, State: owner.PlaceActive}); err != nil {
		t.Fatal(err)
	}
	r.wait(t, "ready", "k:1", "k:1", "k:2", "k:2", "k:2", "z:3")
}

// Every committed change of a database's registry state or target notifies its key on StateChannel; a CAS that
// changes nothing, and the insert, do not. Locate returns the target while moving.
func TestListenNotifiesRegistryStateChanges(t *testing.T) {
	p, _ := newPlacement(t, nil)
	tenant := "s" + uniq()
	r := &recorder{prefix: tenant + ":"}
	startListener(t, p, r)
	r.wait(t, "ready")
	if _, err := p.CreateDatabase(ctx, tenant, "main"); err != nil {
		t.Fatal(err)
	}
	key := tenant + ":main"
	must(t, p.SetDatabaseState(ctx, key, "creating", "active", ""))
	if err := p.SetDatabaseState(ctx, key, "creating", "active", ""); !errors.Is(err, owner.ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	must(t, p.SetDatabaseState(ctx, key, "active", "moving", "vm-b"))
	l, err := p.Locate(ctx, key)
	if err != nil || l.Intent != "moving" || l.Target != "vm-b" {
		t.Fatalf("Locate = %+v, %v", l, err)
	}
	must(t, p.SetDatabaseState(ctx, key, "moving", "moving", "vm-c")) // a new target alone notifies too
	must(t, p.SetDatabaseState(ctx, key, "moving", "active", ""))
	r.wait(t, "ready", "state main", "state main", "state main", "state main")
	if l, err := p.Locate(ctx, key); err != nil || l.Intent != "active" || l.Target != "" {
		t.Fatalf("Locate = %+v, %v", l, err)
	}
}

// PG-10: killing the listener's connection → Lost, reconnect, Ready again, and later changes arrive.
func TestListenReconnectsAfterTheConnectionIsKilled(t *testing.T) {
	app := "test/listen-" + uniq()
	p, _ := newPlacement(t, func(c *Config) { c.AppName = app })
	r := &recorder{prefix: "r" + uniq() + "-"}
	startListener(t, p, r)
	r.wait(t, "ready")
	c := adminConn(t, "")
	var n int
	if err := c.QueryRow(ctx, "select count(pg_terminate_backend(pid)) from pg_stat_activity where application_name = $1", app+"/listen").Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminated %d listeners: %v", n, err)
	}
	r.wait(t, "ready", "lost", "ready")
	if err := p.CAS(ctx, r.prefix+"k", owner.Place{}, owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceActive}); err != nil {
		t.Fatal(err)
	}
	r.wait(t, "ready", "lost", "ready", "k:1")
}

func TestParsePayload(t *testing.T) {
	for _, tc := range []struct {
		in    string
		key   string
		epoch uint64
		ok    bool
	}{
		{"acme:db:3", "acme:db", 3, true},
		{"k:18446744073709551615", "k", 1<<64 - 1, true},
		{"k:0", "", 0, false},
		{":3", "", 0, false},
		{"k3", "", 0, false},
		{"k:x", "", 0, false},
		{"k:", "", 0, false},
	} {
		key, epoch, ok := parsePayload(tc.in)
		if key != tc.key || epoch != tc.epoch || ok != tc.ok {
			t.Errorf("%q: %q %d %v", tc.in, key, epoch, ok)
		}
	}
}
