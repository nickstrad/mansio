package pg

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/owner/ownertest"
)

func TestConfigNormalize(t *testing.T) {
	c := Config{DSN: "postgres://u@h/db"}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	// RI §P Postgres sheet.
	if c.ConnectTimeout != 3*time.Second || c.StatementTimeout != 2*time.Second || c.LockTimeout != time.Second ||
		c.IdleInTxTimeout != 5*time.Second || c.CallTimeout != 2500*time.Millisecond || c.MaxConns != 20 || c.AppName != "placement" {
		t.Fatalf("defaults = %+v", c)
	}
	for name, mod := range map[string]func(*Config){
		"empty dsn":               func(c *Config) { c.DSN = "" },
		"schema with a quote":     func(c *Config) { c.Schema = `a"b` },
		"schema uppercase":        func(c *Config) { c.Schema = "A" },
		"max conns negative":      func(c *Config) { c.MaxConns = -1 },
		"lock = statement":        func(c *Config) { c.LockTimeout, c.StatementTimeout = time.Second, time.Second },
		"call = statement":        func(c *Config) { c.CallTimeout, c.StatementTimeout = time.Second, time.Second },
		"statement below 1 ms":    func(c *Config) { c.StatementTimeout = time.Microsecond },
		"negative keepalive":      func(c *Config) { c.KeepAlive = -1 },
		"breaker failure rate >1": func(c *Config) { c.Breaker.FailureRate = 1.5 },
	} {
		c := Config{DSN: "postgres://u@h/db"}
		mod(&c)
		if err := c.Normalize(); err == nil {
			t.Errorf("%s: Normalize accepted %+v", name, c)
		}
	}
	// On points: lock just below statement, call just above.
	c = Config{DSN: "postgres://u@h/db", StatementTimeout: time.Second, LockTimeout: time.Second - time.Millisecond, CallTimeout: time.Second + time.Millisecond}
	if err := c.Normalize(); err != nil {
		t.Fatalf("on points rejected: %v", err)
	}
}

func TestOpenRejectsReplicaFallbacksAndBadDSN(t *testing.T) {
	for _, dsn := range []string{"postgres://u@h1:5432,h2:5432/db", "postgres://u@[::1"} {
		if _, err := Open(Config{DSN: dsn}); err == nil {
			t.Errorf("Open(%q) succeeded", dsn)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the DSN: %v", err)
		}
	}
	if _, err := Open(Config{DSN: "postgres://u:secret@[::1"}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a bad DSN's error must not quote it: %v", err)
	}
}

func TestPlacementCAS(t *testing.T) {
	pgURL(t)
	ownertest.RunPlacementSuite(t, func(t *testing.T) owner.Placement {
		p, _ := newPlacement(t, nil)
		return p
	})
}

func TestMigrateIsIdempotentAndRefusesNewerSchemas(t *testing.T) {
	p, schema := newPlacement(t, nil) // migrated once
	ms, err := migrations()
	must(t, err)
	if v, err := p.Migrate(ctx); err != nil || v != len(ms) {
		t.Fatalf("second Migrate = %d, %v; want a no-op at %d", v, err, len(ms))
	}
	c := adminConn(t, schema)
	var n int
	if err := c.QueryRow(ctx, "select count(*) from schema_version").Scan(&n); err != nil || n != len(ms) {
		t.Fatalf("schema_version rows = %d, %v", n, err)
	}
	if _, err := c.Exec(ctx, "insert into schema_version (version) values (99)"); err != nil {
		t.Fatal(err)
	}
	if v, err := p.Migrate(ctx); err == nil || v != 99 {
		t.Fatalf("Migrate on a newer schema = %d, %v; want a refusal", v, err)
	}
}

func TestMigrationsAreNumberedWithoutGaps(t *testing.T) {
	ms, err := migrations()
	if err != nil || len(ms) == 0 || ms[0].name != "0001_placement.sql" {
		t.Fatalf("migrations = %+v, %v", ms, err)
	}
	for _, m := range ms {
		if !strings.Contains(m.sql, "begin;") || !strings.Contains(m.sql, "commit;") ||
			!strings.Contains(m.sql, fmt.Sprintf("insert into schema_version (version) values (%d)", m.version)) {
			t.Errorf("%s must be one transaction that records its version", m.name)
		}
	}
}

// The pool's sessions carry the RI §P settings (SQL G-5, G-6).
func TestSessionSettings(t *testing.T) {
	p, _ := newPlacement(t, func(c *Config) { c.AppName = "controld/placement" })
	for name, want := range map[string]string{
		"statement_timeout":                   "2s",
		"lock_timeout":                        "1s",
		"idle_in_transaction_session_timeout": "5s",
		"application_name":                    "controld/placement",
		"transaction_isolation":               "read committed",
		"synchronous_commit":                  "on",
	} {
		var got string
		if err := p.pool.QueryRow(ctx, "select current_setting($1)", name).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

// Input is validated at the edge: nothing is sent, the breaker does not count it, and an unregistered VM is
// the schema's answer, not an outage.
func TestEdgeValidationAndAnswers(t *testing.T) {
	p, _ := newPlacement(t, nil)
	for _, key := range []string{"", strings.Repeat("k", owner.MaxKeyLen+1), "a\x00b", "\xff"} {
		if _, err := p.Get(ctx, key); !errors.Is(err, owner.ErrInvalid) {
			t.Errorf("Get(%q) = %v, want ErrInvalid", key, err)
		}
		if err := p.CAS(ctx, key, owner.Place{}, a1); !errors.Is(err, owner.ErrInvalid) {
			t.Errorf("CAS(%q) = %v, want ErrInvalid", key, err)
		}
	}
	for _, next := range []owner.Place{
		{Owner: "VM-A", Epoch: 1, State: owner.PlaceActive},
		{Owner: "vm-a", Epoch: 1 << 63, State: owner.PlaceActive},
		{Owner: "vm-a", Epoch: 1, Gen: 1 << 63, State: owner.PlaceActive},
		{Owner: "vm-a", Epoch: 1},
	} {
		if err := p.CAS(ctx, "k", owner.Place{}, next); !errors.Is(err, owner.ErrInvalid) {
			t.Errorf("CAS(next %+v) = %v, want ErrInvalid", next, err)
		}
	}
	if err := p.RegisterVM(ctx, "Bad VM", "x"); !errors.Is(err, owner.ErrInvalid) {
		t.Errorf("RegisterVM(bad) = %v, want ErrInvalid", err)
	}
	// On points of the integer ranges are accepted.
	big := owner.Place{Owner: "vm-a", Epoch: 1<<63 - 1, Gen: 1<<63 - 1, State: owner.PlaceActive}
	if err := p.CAS(ctx, "big", owner.Place{}, big); err != nil {
		t.Fatalf("CAS at the max epoch and generation: %v", err)
	}
	if got, err := p.Get(ctx, "big"); err != nil || got != big {
		t.Fatalf("Get(big) = %+v, %v", got, err)
	}
	// An expect that cannot be stored conflicts; an expect.Epoch of 0 with other fields set conflicts.
	if err := p.CAS(ctx, "big", owner.Place{Owner: "VM", Epoch: 1}, a1); !errors.Is(err, owner.ErrConflict) {
		t.Errorf("CAS from an impossible expect = %v, want ErrConflict", err)
	}
	if err := p.CAS(ctx, "new", owner.Place{Owner: "vm-a"}, a1); !errors.Is(err, owner.ErrConflict) {
		t.Errorf("create with a half-set expect = %v, want ErrConflict", err)
	}
	// Answers from the schema.
	if err := p.CAS(ctx, "k", owner.Place{}, owner.Place{Owner: "vm-z", Epoch: 1, State: owner.PlaceActive}); !errors.Is(err, ErrUnknownVM) || errors.Is(err, ErrUnavailable) {
		t.Errorf("CAS to an unregistered vm = %v, want ErrUnknownVM", err)
	}
	must(t, p.CAS(ctx, "k", owner.Place{}, a1))
	err := p.CAS(ctx, "k", a1, owner.Place{Owner: "vm-b", Epoch: 1, State: owner.PlaceActive})
	if !errors.Is(err, ErrInvariant) || errors.Is(err, ErrUnavailable) || errors.Is(err, owner.ErrConflict) {
		t.Errorf("owner change without an epoch bump = %v, want ErrInvariant only", err)
	}
	if !strings.Contains(err.Error(), "placement_epoch_monotonic") && !strings.Contains(err.Error(), "epoch") {
		t.Errorf("trigger error lost its text: %v", err)
	}
	if st := p.Breaker().State(time.Now()); st != breaker.Closed {
		t.Fatalf("answers and bad input moved the breaker to %s", st)
	}
}

// A flip records exactly one moves row with the acknowledged generation; other changes record none; the
// listing queries see the rows.
func TestFlipRecordsItsMove(t *testing.T) {
	p, schema := newPlacement(t, nil)
	a := func(vm string, e owner.Epoch, g owner.Generation, s owner.PlaceState) owner.Place {
		return owner.Place{Owner: vm, Epoch: e, Gen: g, State: s}
	}
	must(t, p.CAS(ctx, "k", owner.Place{}, a("vm-a", 1, 0, owner.PlaceActive)))
	must(t, p.CAS(ctx, "k", a("vm-a", 1, 0, owner.PlaceActive), a("vm-a", 1, 2, owner.PlaceActive))) // gen bump
	must(t, p.CAS(ctx, "k", a("vm-a", 1, 2, owner.PlaceActive), a("vm-a", 1, 2, owner.PlaceMoving))) // begin
	must(t, p.CAS(ctx, "k", a("vm-a", 1, 2, owner.PlaceMoving), a("vm-a", 2, 2, owner.PlaceActive))) // abort to self
	must(t, p.CAS(ctx, "k", a("vm-a", 2, 2, owner.PlaceActive), a("vm-a", 2, 2, owner.PlaceMoving))) // begin
	must(t, p.CAS(ctx, "k", a("vm-a", 2, 2, owner.PlaceMoving), a("vm-b", 3, 5, owner.PlaceActive))) // flip
	must(t, p.CAS(ctx, "m", owner.Place{}, a("vm-c", 1, 0, owner.PlaceMoving)))

	c := adminConn(t, schema)
	rows, err := c.Query(ctx, "select database_id, from_epoch, from_vm, to_vm, ack_generation from moves")
	must(t, err)
	type move struct {
		Key      string
		From     int64
		FVM, TVM string
		Ack      int64
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[move])
	must(t, err)
	if len(got) != 1 || got[0] != (move{"k", 2, "vm-a", "vm-b", 5}) {
		t.Fatalf("moves = %+v, want exactly the flip k@2 vm-a→vm-b ack 5", got)
	}
	if v, err := p.Invariants(ctx, 10); err != nil || len(v) != 0 {
		t.Fatalf("Invariants = %+v, %v", v, err)
	}
	stuck, err := p.Moving(ctx, 0, 10)
	if err != nil || len(stuck) != 1 || stuck[0] != (Stuck{Key: "m", Owner: "vm-c", Epoch: 1}) {
		t.Fatalf("Moving = %+v, %v", stuck, err)
	}
	if stuck, err := p.Moving(ctx, time.Hour, 10); err != nil || len(stuck) != 0 {
		t.Fatalf("Moving(older than 1h) = %+v, %v", stuck, err)
	}
	load, err := p.Load(ctx, 100)
	must(t, err)
	counts := map[string]int64{}
	for _, l := range load {
		counts[l.VM] = l.ActiveDBs
	}
	if counts["vm-b"] != 1 || counts["vm-a"] != 0 || counts["vm-c"] != 0 || len(load) != len(testVMs()) || load[len(load)-1].VM != "vm-b" {
		t.Fatalf("Load = %+v", load)
	}
	if load, err := p.Load(ctx, 2); err != nil || len(load) != 2 {
		t.Fatalf("Load(limit 2) = %+v, %v", load, err)
	}
	// A broken invariant shows up: a moves row whose placement epoch did not move past it.
	_, err = c.Exec(ctx, "insert into moves (database_id, from_epoch, from_vm, to_vm, ack_generation) values ('k', 3, 'vm-b', 'vm-a', 5)")
	must(t, err)
	if v, err := p.Invariants(ctx, 10); err != nil || len(v) != 1 || v[0] != (Violation{Key: "k", FromEpoch: 3, Epoch: 3}) {
		t.Fatalf("Invariants after a bad row = %+v, %v", v, err)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidVMMatchesTheDomain(t *testing.T) {
	domain := regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`) // 0001_placement.sql, vm_id
	for _, vm := range []string{"", "a", "0", "vm-a", "a.b", "-a", ".a", "A", "a_b", "a b", "é",
		strings.Repeat("a", 63), strings.Repeat("a", 64), "a" + strings.Repeat("-", 62)} {
		if ValidVM(vm) != domain.MatchString(vm) {
			t.Errorf("ValidVM(%q) = %v, the domain says %v", vm, ValidVM(vm), domain.MatchString(vm))
		}
	}
}
