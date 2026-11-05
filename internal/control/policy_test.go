package control

import (
	"errors"
	"testing"
	"time"
)

func TestPolicyPick(t *testing.T) {
	l := func(vm string, dbs, bytes int64, age time.Duration) VMLoad {
		return VMLoad{VM: vm, ActiveDBs: dbs, Bytes: bytes, HeartbeatAge: age}
	}
	capped := Policy{MaxDatabases: 10, MaxBytes: 1000, MaxHeartbeatAge: 10 * time.Second}
	for _, tc := range []struct {
		name    string
		p       Policy
		loads   []VMLoad
		exclude []string
		want    string
	}{
		{"none", capped, nil, nil, ""},
		{"fewest databases", Policy{}, []VMLoad{l("a", 3, 0, 0), l("b", 2, 0, 0)}, nil, "b"},
		{"tie on databases: fewer bytes", Policy{}, []VMLoad{l("a", 2, 50, 0), l("b", 2, 40, 0)}, nil, "b"},
		{"full tie: vm id", Policy{}, []VMLoad{l("b", 2, 40, 0), l("a", 2, 40, 0)}, nil, "a"},
		{"dominant share: bytes outweigh databases", capped, []VMLoad{l("a", 1, 900, 0), l("b", 5, 100, 0)}, nil, "b"},
		{"database cap on point (9 < 10)", capped, []VMLoad{l("a", 9, 0, 0)}, nil, "a"},
		{"database cap off point (10)", capped, []VMLoad{l("a", 10, 0, 0)}, nil, ""},
		{"bytes cap off point (1000)", capped, []VMLoad{l("a", 0, 1000, 0)}, nil, ""},
		{"bytes cap on point (999)", capped, []VMLoad{l("a", 0, 999, 0)}, nil, "a"},
		{"heartbeat age on point (10 s)", capped, []VMLoad{l("a", 0, 0, 10 * time.Second)}, nil, "a"},
		{"heartbeat age off point (10 s + 1 ns)", capped, []VMLoad{l("a", 0, 0, 10*time.Second + 1)}, nil, ""},
		{"stale VM skipped for a busier live one", capped, []VMLoad{l("a", 0, 0, time.Minute), l("b", 8, 0, 0)}, nil, "b"},
		{"excluded", Policy{}, []VMLoad{l("a", 0, 0, 0), l("b", 5, 0, 0)}, []string{"a"}, "b"},
		{"no caps: zero share for all", Policy{}, []VMLoad{l("a", 1000, 1 << 40, 0)}, nil, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.p.Pick(tc.loads, tc.exclude...)
			if tc.want == "" {
				if !errors.Is(err, ErrNoCapacity) {
					t.Fatalf("Pick = %q, %v; want ErrNoCapacity", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Pick = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// Placing one database at a time on the pick spreads evenly: 100 creates over two empty VMs end 50/50.
func TestPolicySpreadsEvenly(t *testing.T) {
	loads := []VMLoad{{VM: "a"}, {VM: "b"}, {VM: "c"}}
	p := Policy{MaxDatabases: 1000}
	for range 99 {
		vm, err := p.Pick(loads)
		if err != nil {
			t.Fatal(err)
		}
		for i := range loads {
			if loads[i].VM == vm {
				loads[i].ActiveDBs++
			}
		}
	}
	for _, l := range loads {
		if l.ActiveDBs != 33 {
			t.Fatalf("uneven: %+v", loads)
		}
	}
}
