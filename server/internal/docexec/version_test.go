package docexec_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/docexec"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/ir"
)

// restart is a kill -9 and the owner's restart: the host is abandoned with its unpublished writes, the manifest
// is stamped (owner.Table.Claim does that before it admits work), and a new host restores the last published
// generation.
func (d *db) restart() {
	d.t.Helper()
	d.store.SetFaults(durabletest.Faults{Down: true})
	d.h.Close()
	d.store.SetFaults(durabletest.Faults{})
	m, _, err := d.store.ReadManifest(context.Background(), durableKey)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, _, err := durable.Stamp(context.Background(), d.store, durableKey, m.Epoch, m.Generation, d.clock.Now()); err != nil {
		d.t.Fatal(err)
	}
	d.newHost()
}

// publish writes the database back: what it holds now survives a restart.
func (d *db) publish() {
	d.t.Helper()
	if res, err := d.h.WriteBack(context.Background(), durableKey); err != nil || !res.Wrote {
		d.t.Fatalf("write-back: %+v %v", res, err)
	}
}

// patchNote patches note id's n, with an expected version if expect is not 0, and answers the new version.
func (d *db) patchNote(rid, id string, n int, expect float64) (float64, *ir.Error) {
	d.t.Helper()
	op := fmt.Sprintf(`{"op":"patch","table":"notes","id":%q,"value":{"n":%d}`, id, n)
	if expect != 0 {
		op += fmt.Sprintf(`,"expect":{"version":%.0f}`, expect)
	}
	got := d.exec(body("mutation", rid, d.schema.Fingerprint, "["+op+"}]"), &listIDs{}, nil)
	if got.err != nil {
		return 0, got.err
	}
	return got.result.([]any)[0].(*ir.Object).Value("_version").(float64), nil
}

// versionOf is the stored _version of note id.
func (d *db) versionOf(id string) float64 {
	d.t.Helper()
	for _, doc := range d.dump()["notes"].([]any) {
		if doc := doc.(*ir.Object); doc.Value("_id") == id {
			return doc.Value("_version").(float64)
		}
	}
	d.t.Fatalf("no note %s", id)
	return 0
}

// The open finding of 2026-11-06 (sim swarm seed 2761; the cluster vector is
// internal/sim/testdata/cluster/version-reuse-after-rollback.json): a database rolled back by a kill -9 must
// not issue the lost writes' versions again. A client that read a document before the crash holds a version of
// contents that no longer exist; its expect must fail against whatever the restored database writes instead.
//
// The second round is the case the proposed floor seal<<32 missed while the seal restarted with each
// generation: the database is published (a new generation) between two crashes.
func TestVersionIsNotReissuedAfterARollback(t *testing.T) {
	s := mustDecode(t, notes)
	d := openDurable(t, s, actor.Config{})
	insert := `[{"op":"insert","table":"notes","value":{"title":"t","n":1}}]`
	if got := d.exec(body("mutation", "r0", s.Fingerprint, insert), &listIDs{ids: []string{"a"}}, nil); got.err != nil {
		t.Fatal(got.err.Message())
	}
	issued := map[float64]int{} // version → the n it named
	for round := range 3 {
		d.publish()
		lost, err := d.patchNote(fmt.Sprintf("lost%d", round), "a", 100+round, 0)
		if err != nil {
			t.Fatal(err.Message())
		}
		issued[lost] = 100 + round
		twin := []string{fmt.Sprintf("twin%d", round)}
		if got := d.exec(body("mutation", fmt.Sprintf("lt%d", round), s.Fingerprint, insert), &listIDs{ids: twin}, nil); got.err != nil {
			t.Fatal(got.err.Message())
		}
		lostTwin := d.versionOf(twin[0])

		d.restart() // both writes are gone

		// The client that read the lost write retries on top of it: refused, with the document as it is.
		if _, err := d.patchNote(fmt.Sprintf("stale%d", round), "a", 999, lost); err == nil || err.Tag != ir.TagConflict || err.Field("check") != "version" {
			t.Fatalf("round %d: a patch expecting the lost version %.0f: %v", round, lost, err)
		}
		// Another write lands where the lost one did: it must not take the lost version.
		now, err := d.patchNote(fmt.Sprintf("new%d", round), "a", 200+round, 0)
		if err != nil {
			t.Fatal(err.Message())
		}
		if n, taken := issued[now]; taken || now <= lost {
			t.Fatalf("round %d: the restored database issued version %.0f (lost: %.0f, which named n=%d)", round, now, lost, n)
		}
		issued[now] = 200 + round
		if _, err := d.patchNote(fmt.Sprintf("stale%db", round), "a", 999, lost); err == nil || err.Tag != ir.TagConflict {
			t.Fatalf("round %d: a patch expecting the lost version %.0f after a new write: %v", round, lost, err)
		}
		// So for an insert: the same id again is a new document at a version the lost one never had.
		if got := d.exec(body("mutation", fmt.Sprintf("nt%d", round), s.Fingerprint, insert), &listIDs{ids: twin}, nil); got.err != nil {
			t.Fatal(got.err.Message())
		}
		if v := d.versionOf(twin[0]); v <= lostTwin {
			t.Fatalf("round %d: the insert after the restart is at version %.0f, the lost one was at %.0f", round, v, lostTwin)
		}
	}
}

// The floor's arithmetic (spec §6.3): no floor without an incarnation; under one, an insert starts at the
// floor and a patch counts on from there; a database stamped 2^21 times has no version left and refuses writes.
func TestVersionFloor(t *testing.T) {
	s := mustDecode(t, notes)
	insert := body("mutation", "r1", s.Fingerprint, `[{"op":"insert","table":"notes","value":{"title":"t","n":1}}]`)
	for _, c := range []struct {
		incarnation    uint64
		insert, second float64
	}{{0, 1, 2}, {1, 1 << 32, 1<<32 + 1}, {3, 3 << 32, 3<<32 + 1}, {1<<21 - 1, (1<<21 - 1) << 32, (1<<21-1)<<32 + 1}} {
		if got := docexec.VersionFloor(c.incarnation); got != float64(c.incarnation<<32) {
			t.Fatalf("VersionFloor(%d) = %v", c.incarnation, got)
		}
		d := openDB(t, s, actor.Config{Incarnation: c.incarnation})
		if got := d.exec(insert, &listIDs{ids: []string{"a"}}, nil); got.err != nil {
			t.Fatalf("incarnation %d: insert: %s", c.incarnation, got.err.Message())
		}
		if v := d.versionOf("a"); v != c.insert {
			t.Fatalf("incarnation %d: inserted at version %v, want %.0f", c.incarnation, v, c.insert)
		}
		for i, want := range []float64{c.second, c.second + 1} {
			if v, err := d.patchNote(fmt.Sprintf("p%d", i), "a", i, want-1); err != nil || v != want {
				t.Fatalf("incarnation %d: patch %d answered version %.0f %v, want %.0f", c.incarnation, i, v, err, want)
			}
		}
	}
	d := openDB(t, s, actor.Config{Incarnation: 1 << 21})
	if got := d.exec(insert, &listIDs{ids: []string{"a"}}, nil); got.err == nil || got.err.Tag != ir.TagStorage {
		t.Fatalf("an insert with no version left: %+v", got)
	}
	if docs := d.dump()["notes"].([]any); len(docs) != 0 {
		t.Fatalf("the refused insert left %v", docs)
	}
}
