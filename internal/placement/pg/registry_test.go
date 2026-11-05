package pg

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/owner"
)

// The Go name checks use the very patterns the migration's domains do.
func TestRegistryNamesMatchTheDomains(t *testing.T) {
	sql, err := os.ReadFile("schema/0002_registry.sql")
	must(t, err)
	for _, pat := range []string{tenantPattern, namePattern} {
		if !strings.Contains(string(sql), "'"+pat+"'") {
			t.Errorf("pattern %s is not a domain of 0002_registry.sql", pat)
		}
	}
	for _, tc := range []struct {
		s            string
		tenant, name bool
	}{
		{"", false, false}, {"a", true, true}, {"a-b", true, true}, {"a_b", false, true}, {"-a", false, false},
		{"A", false, false}, {"a:b", false, false}, {strings.Repeat("a", 56), true, true},
		{strings.Repeat("a", 57), true, false}, {strings.Repeat("a", 63), true, false}, {strings.Repeat("a", 64), false, false},
	} {
		if ValidTenant(tc.s) != tc.tenant || ValidName(tc.s) != tc.name {
			t.Errorf("%q: tenant %v name %v, want %v %v", tc.s, ValidTenant(tc.s), ValidName(tc.s), tc.tenant, tc.name)
		}
	}
}

// The registries: create is idempotent and makes the tenant's first database its default; state changes are
// CAS; pending lists creates, moves and drops; moving carries the intent; heartbeats and attestations update the VM
// row; owned-by pages by key; dropping the placement row takes its moves rows along.
func TestRegistries(t *testing.T) {
	p, _ := newPlacement(t, nil)
	d, err := p.CreateDatabase(ctx, "acme", "main")
	must(t, err)
	if d != (Database{Key: "acme:main", Tenant: "acme", Name: "main", Default: true, State: "creating"}) {
		t.Fatalf("first create = %+v", d)
	}
	again, err := p.CreateDatabase(ctx, "acme", "main")
	if err != nil || again != d {
		t.Fatalf("second create = %+v, %v", again, err)
	}
	second, err := p.CreateDatabase(ctx, "acme", "logs")
	if err != nil || second.Default {
		t.Fatalf("second database of a tenant = %+v, %v; want not default", second, err)
	}
	if _, err := p.CreateDatabase(ctx, "Acme", "x"); !errors.Is(err, owner.ErrInvalid) {
		t.Fatalf("bad tenant: %v", err)
	}
	if _, err := p.GetDatabase(ctx, "acme:none"); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("missing database: %v", err)
	}

	pending, err := p.Pending(ctx, 0, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	must(t, p.SetDatabaseState(ctx, "acme:main", "creating", "active", ""))
	if err := p.SetDatabaseState(ctx, "acme:main", "creating", "active", ""); !errors.Is(err, owner.ErrConflict) {
		t.Fatalf("stale state CAS: %v", err)
	}
	if err := p.SetDatabaseState(ctx, "acme:main", "active", "active", "vm-a"); !errors.Is(err, ErrInvariant) {
		t.Fatalf("target outside moving: %v", err)
	}
	must(t, p.SetDatabaseState(ctx, "acme:main", "active", "moving", "vm-b"))
	if pending, _ := p.Pending(ctx, 0, 10); len(pending) != 2 || pending[0].Key != "acme:logs" || pending[1].Key != "acme:main" {
		t.Fatalf("Pending after create = %+v", pending)
	}
	if pending, _ := p.Pending(ctx, time.Hour, 10); len(pending) != 0 {
		t.Fatalf("Pending(older than 1h) = %+v", pending)
	}

	row := owner.Place{Owner: "vm-a", Epoch: 1, State: owner.PlaceActive}
	must(t, p.CAS(ctx, "acme:main", owner.Place{}, row))
	mv := row
	mv.State = owner.PlaceMoving
	must(t, p.CAS(ctx, "acme:main", row, mv))
	stuck, err := p.Moving(ctx, 0, 10)
	if err != nil || len(stuck) != 1 || stuck[0] != (Stuck{Key: "acme:main", Owner: "vm-a", Epoch: 1, Intent: "moving", Target: "vm-b"}) {
		t.Fatalf("Moving = %+v, %v", stuck, err)
	}
	flipped := owner.Place{Owner: "vm-b", Epoch: 2, State: owner.PlaceMoving}
	must(t, p.CAS(ctx, "acme:main", mv, owner.Place{Owner: "vm-b", Epoch: 2, State: owner.PlaceActive}))
	must(t, p.CAS(ctx, "acme:main", owner.Place{Owner: "vm-b", Epoch: 2, State: owner.PlaceActive}, flipped))

	// Owned-by pages in key order.
	for _, k := range []string{"k1", "k2", "k3"} {
		must(t, p.CAS(ctx, k, owner.Place{}, owner.Place{Owner: "vm-c", Epoch: 1, State: owner.PlaceActive}))
	}
	page, err := p.OwnedBy(ctx, "vm-c", "", 2)
	if err != nil || len(page) != 2 || page[0].Key != "k1" || page[1].Key != "k2" {
		t.Fatalf("OwnedBy page 1 = %+v, %v", page, err)
	}
	if page, err = p.OwnedBy(ctx, "vm-c", "k2", 2); err != nil || len(page) != 1 || page[0].Key != "k3" || page[0].Place.Owner != "vm-c" {
		t.Fatalf("OwnedBy page 2 = %+v, %v", page, err)
	}

	// Drop: only the expected MOVING row, and its moves rows with it.
	if err := p.DropPlacement(ctx, "acme:main", mv); !errors.Is(err, owner.ErrConflict) {
		t.Fatalf("drop of a stale row: %v", err)
	}
	must(t, p.DropPlacement(ctx, "acme:main", flipped))
	if _, err := p.Get(ctx, "acme:main"); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("placement after drop: %v", err)
	}
	if v, err := p.Invariants(ctx, 10); err != nil || len(v) != 0 {
		t.Fatalf("moves rows left behind: %+v, %v", v, err)
	}

	// VM registry: heartbeat, attestation (idempotent, first attester stays), load excludes the dead.
	must(t, p.Heartbeat(ctx, "vm-a", 4096))
	if err := p.Heartbeat(ctx, "vm-zz", 1); !errors.Is(err, ErrUnknownVM) {
		t.Fatalf("heartbeat of an unknown vm: %v", err)
	}
	v, err := p.VM(ctx, "vm-a")
	if err != nil || v.Bytes != 4096 || v.Dead || v.HeartbeatAge > 5 {
		t.Fatalf("VM = %+v, %v", v, err)
	}
	must(t, p.AttestDead(ctx, "vm-a", "alice"))
	must(t, p.AttestDead(ctx, "vm-a", "bob"))
	if v, _ := p.VM(ctx, "vm-a"); !v.Dead || v.DeadAttestedBy != "alice" {
		t.Fatalf("after attestation: %+v", v)
	}
	if err := p.AttestDead(ctx, "vm-a", ""); !errors.Is(err, owner.ErrInvalid) {
		t.Fatalf("attestation without attester: %v", err)
	}
	if _, err := p.VM(ctx, "vm-zz"); !errors.Is(err, ErrUnknownVM) {
		t.Fatalf("unknown vm: %v", err)
	}
	load, err := p.Load(ctx, 100)
	must(t, err)
	for _, l := range load {
		if l.VM == "vm-a" {
			t.Fatalf("Load lists a VM attested dead: %+v", load)
		}
		if l.VM == "vm-c" && (l.ActiveDBs != 3 || l.HeartbeatAge > 5) {
			t.Fatalf("Load for vm-c = %+v", l)
		}
	}
}
