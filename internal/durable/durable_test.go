package durable_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dg  = durabletest.DigestOf([]byte("data"))
)

func TestValidateKey(t *testing.T) {
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{"a", true},                       // shortest (on point)
		{strings.Repeat("k", 120), true},  // longest (on point): durable.MaxKeyLen
		{strings.Repeat("k", 121), false}, // off point
		{"", false},                       // off point
		{"tenant/db-1", true},
		{"/a", false}, {"a/", false}, {"a//b", false},
		{".", false}, {"a/..", false}, {"a/./b", false}, {"..a", true},
		{`a\b`, false}, {"a\x00b", false},
	} {
		if err := durable.ValidateKey(tc.key); (err == nil) != tc.ok {
			t.Errorf("ValidateKey(%q) = %v, want ok=%v", tc.key, err, tc.ok)
		}
	}
}

func TestLayout(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{durable.KeyPrefix("t1/a"), "dbs/bb/t1%2Fa/"}, // "/" is escaped: one path segment per key
		{durable.KeyPrefix("t1"), "dbs/62/t1/"},
		{durable.KeyPrefix("a b%"), "dbs/bc/a%20b%25/"},
		{durable.GenObject("t/a", 42, 3), "dbs/84/t%2Fa/gen/00000000000000000042-e00000000000000000003.db"},
		{durable.GenObject("a", 1<<64-1, 1<<64-1), "dbs/ca/a/gen/18446744073709551615-e18446744073709551615.db"}, // 20 digits fit uint64
		{durable.ManifestObject("t/a"), "dbs/84/t%2Fa/manifest.json"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	if len(durable.ManifestObject(strings.Repeat("/", durable.MaxKeyLen))) > 1024 {
		t.Fatal("the longest escaped key's object name is over S3's 1024-byte limit")
	}
}

// FuzzKeyPrefixDisjoint (OS-04, guide K1): for distinct valid keys a and b, no object of a lies under
// KeyPrefix(b), so prefix-based GC, drop and inventory never reach another database.
func FuzzKeyPrefixDisjoint(f *testing.F) {
	f.Add("t1", "t1/gen/x")
	f.Add("a", "a/b")
	f.Add("a%2Fb", "a/b")
	f.Add("a b", "a%20b")
	f.Add("x", "x")
	f.Fuzz(func(t *testing.T, a, b string) {
		if a == b || durable.ValidateKey(a) != nil || durable.ValidateKey(b) != nil {
			return
		}
		pb := durable.KeyPrefix(b)
		for _, name := range []string{durable.KeyPrefix(a), durable.ManifestObject(a), durable.GenObject(a, 1, 1)} {
			if strings.HasPrefix(name, pb) {
				t.Fatalf("%q (of key %q) is under KeyPrefix(%q) = %q", name, a, b, pb)
			}
		}
		if !strings.HasPrefix(durable.ManifestObject(a), durable.KeyPrefix(a)) ||
			!strings.HasPrefix(durable.GenObject(a, 1, 1), durable.KeyPrefix(a)) {
			t.Fatalf("an object of %q is outside its own prefix", a)
		}
	})
}

func TestManifestValidateAndDecode(t *testing.T) {
	good := durable.NextManifest("k", 3, 2, dg, t0)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	back, err := durable.Decode("k", durable.Encode(good))
	if err != nil || back != good {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if !strings.Contains(string(durable.Encode(good)), `"written_at":"2026-11-05T12:00:00Z"`) {
		t.Fatalf("JSON field names: %s", durable.Encode(good))
	}
	if _, err := durable.Decode("other", durable.Encode(good)); err == nil {
		t.Fatal("Decode accepted a manifest stored under another key")
	}
	if _, err := durable.Decode("k", []byte("{")); err == nil {
		t.Fatal("Decode accepted bad JSON")
	}
	empty := durable.Manifest{Key: "k", Epoch: 1}
	if err := empty.Validate(); err != nil {
		t.Fatalf("generation 0 with no object: %v", err)
	}
	noMD5 := good
	noMD5.MD5 = "" // optional: older writers and callers that did not compute it
	if err := noMD5.Validate(); err != nil {
		t.Fatalf("manifest without md5: %v", err)
	}
	if empty.MD5 = good.MD5; empty.Validate() == nil {
		t.Fatal("generation 0 with an md5 accepted")
	}
	for name, mut := range map[string]func(*durable.Manifest){
		"gen 0 with object":  func(m *durable.Manifest) { m.Generation = 0 },
		"object name":        func(m *durable.Manifest) { m.Object = durable.GenObject("k", 4, 2) },
		"other key's object": func(m *durable.Manifest) { m.Object = durable.GenObject("j", 3, 2) },
		"old layout name":    func(m *durable.Manifest) { m.Object = durable.KeyPrefix("k") + "gen/00000000000000000003.db" },
		"epoch digits":       func(m *durable.Manifest) { m.Object = strings.Replace(m.Object, "-e0", "-ex", 1) },
		"short epoch":        func(m *durable.Manifest) { m.Object = strings.Replace(m.Object, "-e0", "-e", 1) },
		"epoch overflow":     func(m *durable.Manifest) { m.Object = overflow(m.Object) },
		"negative size":      func(m *durable.Manifest) { m.Size = -1 },
		"short hash":         func(m *durable.Manifest) { m.SHA256 = m.SHA256[:63] },
		"upper-case hash":    func(m *durable.Manifest) { m.SHA256 = strings.ToUpper(m.SHA256) },
		"short md5":          func(m *durable.Manifest) { m.MD5 = m.MD5[:31] },
		"upper-case md5":     func(m *durable.Manifest) { m.MD5 = strings.ToUpper(m.MD5) },
		"bad key":            func(m *durable.Manifest) { m.Key = "" },
	} {
		m := good
		mut(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, m)
		}
	}
}

// overflow replaces the 20-digit epoch in object with 99999999999999999999 (> 2^64-1).
func overflow(object string) string {
	i := strings.Index(object, "-e")
	return object[:i+2] + strings.Repeat("9", 20) + ".db"
}

// Validate's epoch rule (OS-05): a manifest may name an object uploaded at its own epoch or an earlier one
// (a Stamp raises the manifest's epoch without renaming the object), never a later one.
func TestValidateObjectEpoch(t *testing.T) {
	for _, tc := range []struct {
		objEpoch, epoch uint64
		ok              bool
	}{
		{2, 2, true},  // on point: uploaded and published at one epoch
		{1, 2, true},  // stamped since the upload
		{0, 0, true},  // epoch 0 (unfenced)
		{3, 2, false}, // off point: named object is from a later epoch
	} {
		m := durable.NextManifest("k", 3, tc.objEpoch, dg, t0)
		m.Epoch = tc.epoch
		if err := m.Validate(); (err == nil) != tc.ok {
			t.Errorf("object epoch %d, manifest epoch %d: Validate = %v, want ok=%v", tc.objEpoch, tc.epoch, err, tc.ok)
		}
	}
}

func TestCheckPublish(t *testing.T) {
	stored := durable.NextManifest("k", 5, 3, dg, t0)
	for _, tc := range []struct {
		name       string
		stored     *durable.Manifest
		gen, epoch uint64
		ok         bool
	}{
		{"first publish", nil, 1, 0, true},
		{"next gen, same epoch (on point)", &stored, 6, 3, true},
		{"next gen, higher epoch", &stored, 6, 4, true},
		{"lower epoch (off point)", &stored, 6, 2, false},
		{"same generation (off point)", &stored, 5, 3, false},
		{"older generation", &stored, 4, 9, false},
		{"skips generations", &stored, 9, 3, true},
	} {
		err := durable.CheckPublish(tc.stored, durable.NextManifest("k", tc.gen, tc.epoch, dg, t0))
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, durable.ErrStale)) {
			t.Errorf("%s: err = %v, want ok=%v (else ErrStale)", tc.name, err, tc.ok)
		}
	}
}

func TestCheckStamp(t *testing.T) {
	stored := durable.NextManifest("k", 5, 3, dg, t0)
	for _, tc := range []struct {
		name       string
		stored     *durable.Manifest
		epoch, gen uint64
		ok         bool
	}{
		{"no manifest, gen 0", nil, 1, 0, true},
		{"no manifest, gen 1", nil, 1, 1, false},
		{"raise epoch", &stored, 4, 5, true},
		{"same epoch (on point)", &stored, 3, 5, true},
		{"lower epoch (off point)", &stored, 2, 5, false},
		{"generation moved", &stored, 4, 4, false},
	} {
		err := durable.CheckStamp(tc.stored, "k", tc.epoch, tc.gen)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, durable.ErrStale)) {
			t.Errorf("%s: err = %v, want ok=%v (else ErrStale)", tc.name, err, tc.ok)
		}
	}
}

func TestStamp(t *testing.T) {
	s := durabletest.NewStore()
	m, v, err := durable.Stamp(ctx, s, "k", 1, 0, t0) // creates a generation-0 manifest
	if err != nil || m.Generation != 0 || m.Epoch != 1 || v == durable.NoVersion {
		t.Fatalf("Stamp on empty = %+v %q %v", m, v, err)
	}
	seeded := s.Seed("k", 1, []byte("g1"))
	m, v2, err := durable.Stamp(ctx, s, "k", 4, seeded.Generation, t0)
	if err != nil || m.Epoch != 4 || !strings.HasSuffix(m.Object, "1.db") || v2 == v {
		t.Fatalf("Stamp = %+v %q %v", m, v2, err)
	}
	if got, _ := s.Manifest("k"); got.Epoch != 4 || got.Generation != seeded.Generation || got.SHA256 != seeded.SHA256 {
		t.Fatalf("stored after stamp %+v", got)
	}
	if again, v3, err := durable.Stamp(ctx, s, "k", 4, seeded.Generation, t0); err != nil || v3 != v2 || again.Epoch != 4 {
		t.Fatalf("re-stamp at the same epoch must be a no-op: %+v %q %v", again, v3, err)
	}
	// A publish at the old epoch now fails: the point of Stamp.
	low := durable.NextManifest("k", 2, 1, dg, t0)
	if _, err := s.PublishManifest(ctx, low, v2); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("publish at a lower epoch after a stamp: %v", err)
	}
	if _, _, err := durable.Stamp(ctx, s, "k", 3, seeded.Generation, t0); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("stamp at a lower epoch: %v", err)
	}
	s.SetFaults(durabletest.Faults{FailRead: true})
	if _, _, err := durable.Stamp(ctx, s, "k", 9, seeded.Generation, t0); err == nil || errors.Is(err, durable.ErrStale) {
		t.Fatalf("stamp with a failing read: %v", err)
	}
	s.SetFaults(durabletest.Faults{FailPublish: true})
	if _, _, err := durable.Stamp(ctx, s, "k", 9, seeded.Generation, t0); err == nil || errors.Is(err, durable.ErrStale) {
		t.Fatalf("stamp with a failing publish: %v", err)
	}
	s.SetFaults(durabletest.Faults{})
	s.Bump("k", 9) // another owner moved it between our read and publish? emulate by racing: stale version
	if _, _, err := durable.Stamp(ctx, s, "k", 8, seeded.Generation, t0); !errors.Is(err, durable.ErrStale) {
		t.Fatalf("stamp below a bumped epoch: %v", err)
	}
}

// A stamp whose publish applied but whose reply was lost (or re-sent and refused with 412) is resolved by a
// re-read (object-storage guide R3): it succeeds. A stamp that did not apply still fails (OS-02).
func TestStampAmbiguousOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		f     durabletest.Faults
		ok    bool
		stale bool
	}{
		{"applied, reply lost", durabletest.Faults{LosePublishReply: true}, true, false},
		{"applied, re-send got 412", durabletest.Faults{ApplyThenStale: true}, true, false},
		{"not applied", durabletest.Faults{FailPublish: true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := durabletest.NewStore()
			seeded := s.Seed("k", 1, []byte("g1"))
			s.SetFaults(tc.f)
			m, v, err := durable.Stamp(ctx, s, "k", 2, seeded.Generation, t0)
			s.SetFaults(durabletest.Faults{})
			stored, sv, _ := s.ReadManifest(ctx, "k")
			switch {
			case tc.ok && (err != nil || !m.Same(stored) || v != sv || m.Epoch != 2):
				t.Fatalf("Stamp = %+v %q %v; stored %+v %q", m, v, err, stored, sv)
			case !tc.ok && (err == nil || errors.Is(err, durable.ErrStale) != tc.stale || stored.Epoch != 1):
				t.Fatalf("Stamp err = %v, stored epoch %d", err, stored.Epoch)
			}
		})
	}
}
