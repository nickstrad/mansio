package docexec_test

import (
	"fmt"
	"testing"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/ir"
)

const notesAndLists = `{"notes":{"fields":{"title":{"node":{"kind":"string"}},"n":{"node":{"kind":"integer"}}},"storage":"columns","indexes":[]},` +
	`"lists":{"fields":{"name":{"node":{"kind":"string"}}},"storage":"columns","indexes":[]}}`

// write runs one mutation that must apply and answers the commit sequence it left.
func (d *db) write(rid, ops string, ids ...string) int64 {
	d.t.Helper()
	got := d.exec(body("mutation", rid, d.schema.Fingerprint, ops), &listIDs{ids: ids}, nil)
	if got.err != nil {
		d.t.Fatalf("%s: %s", rid, got.err.Message())
	}
	return got.seq
}

// guard asks, in a query, whether table is unchanged since seq (spec §5.5): the answer's sequence, or the
// conflict.
func (d *db) guard(table string, since int64) (int64, *ir.Error) {
	d.t.Helper()
	got := d.exec(body("query", "", d.schema.Fingerprint, fmt.Sprintf(`[{"op":"assert","table":%q,"expect":{"unchangedSince":%d}}]`, table, since)), &listIDs{}, nil)
	return got.seq, got.err
}

// refused fails the test unless a guard on table since seq is a ConflictError of the table check.
func (d *db) refused(when, table string, since int64) {
	d.t.Helper()
	if _, err := d.guard(table, since); err == nil || err.Tag != ir.TagConflict || err.Field("check") != "unchanged" {
		d.t.Fatalf("%s: %s unchanged since the lost sequence %d: %v, want a conflict", when, table, since, err)
	}
}

// The open finding of the version-floor note (2026-11-10): a database rolled back by a kill -9 must not issue
// the lost mutations' sequences again, and a table precondition taken on the lost state must not hold against
// the restored one. A client that ran a find before the crash holds a sequence of a table that no longer
// exists in that form; its assert {unchangedSince} must conflict: before anything is written again (the
// database never reached that sequence), after a write to that table (which must not land on the lost
// sequence), and after a write to another table only (the guarded table's own last change is old and low).
//
// As in TestVersionIsNotReissuedAfterARollback, the database is published between the crashes.
func TestSequenceIsNotReissuedAfterARollback(t *testing.T) {
	s := mustDecode(t, notesAndLists)
	d := openDurable(t, s, actor.Config{})
	note := `[{"op":"insert","table":"notes","value":{"title":"t","n":1}}]`
	list := `[{"op":"insert","table":"lists","value":{"name":"l"}}]`
	d.write("r0", note, "a")
	issued := map[int64]string{} // sequence → the mutation that left it
	for round := range 3 {
		d.publish()
		kept, err := d.guard("notes", 0)
		if err == nil {
			t.Fatalf("round %d: notes unchanged since 0 holds after an insert", round)
		}
		kept = int64(err.Field("seq").(float64))

		// Lost: a note and a list. The client's find answers at the sequence these leave.
		d.write(fmt.Sprintf("ln%d", round), note, fmt.Sprintf("lost%d", round))
		lost := d.write(fmt.Sprintf("ll%d", round), list, fmt.Sprintf("lostlist%d", round))
		if seq, err := d.guard("notes", lost); err != nil || seq != lost {
			t.Fatalf("round %d: the guard before the crash: seq %d %v, want %d", round, seq, err, lost)
		}
		issued[lost-1], issued[lost] = "a lost note", "a lost list"

		d.restart() // both writes are gone

		// Nothing written yet: the database is back at the published sequence, and what held there holds.
		if seq, err := d.guard("notes", kept); err != nil || seq != kept {
			t.Fatalf("round %d: after the restart, a guard at the published sequence %d: seq %d %v", round, kept, seq, err)
		}
		d.refused(fmt.Sprintf("round %d, before a new write", round), "notes", lost)
		d.refused(fmt.Sprintf("round %d, before a new write", round), "lists", lost)

		// Another note lands where the lost one did. It must not take a lost sequence.
		now := d.write(fmt.Sprintf("nn%d", round), note, fmt.Sprintf("new%d", round))
		if what, taken := issued[now]; taken || now <= lost {
			t.Fatalf("round %d: the restored database issued sequence %d (lost: %d, which was %s)", round, now, lost, what)
		}
		issued[now] = "a new note"
		d.refused(fmt.Sprintf("round %d, after a new note", round), "notes", lost)
		// lists was not written since the restore, but the client saw the lost list in it.
		d.refused(fmt.Sprintf("round %d, after a new note", round), "lists", lost)
		// A guard taken now holds: the sequence works as before inside an incarnation.
		for _, table := range []string{"notes", "lists"} {
			if seq, err := d.guard(table, now); err != nil || seq != now {
				t.Fatalf("round %d: %s unchanged since %d, just answered: seq %d %v", round, table, now, seq, err)
			}
		}
		if next := d.write(fmt.Sprintf("nl%d", round), list, fmt.Sprintf("newlist%d", round)); next != now+1 {
			t.Fatalf("round %d: the mutation after %d answered %d", round, now, next)
		} else {
			issued[next] = "a new list"
		}
		if _, err := d.guard("notes", now); err != nil {
			t.Fatalf("round %d: notes unchanged since %d after a write to lists: %s", round, now, err.Message())
		}
	}
}

// The sequence's floor (spec §6.5): none without an incarnation; under one, the first mutation that changes a
// row answers the floor and marks every table changed there, and the count goes on by one; a sequence the
// database never reached is refused; a database stamped 2^21 times has no sequence left and refuses writes.
func TestSequenceFloor(t *testing.T) {
	s := mustDecode(t, notesAndLists)
	note := `[{"op":"insert","table":"notes","value":{"title":"t","n":1}}]`
	for _, c := range []struct {
		incarnation uint64
		first       int64
	}{{0, 1}, {1, 1 << 32}, {3, 3 << 32}, {1<<21 - 1, (1<<21 - 1) << 32}} {
		d := openDB(t, s, actor.Config{Incarnation: c.incarnation})
		if seq, err := d.guard("lists", 0); err != nil || seq != 0 {
			t.Fatalf("incarnation %d: a new database: seq %d %v", c.incarnation, seq, err)
		}
		if _, err := d.guard("lists", 1); err == nil || err.Field("actual") != 0.0 || err.Field("seq") != 0.0 {
			t.Fatalf("incarnation %d: a guard above the database's sequence: %v", c.incarnation, err)
		}
		if seq := d.write("r1", note, "a"); seq != c.first {
			t.Fatalf("incarnation %d: the first mutation answered %d, want %d", c.incarnation, seq, c.first)
		}
		// lists was not written, but under an incarnation it counts as changed at the floor.
		_, err := d.guard("lists", c.first-1)
		if marked := err != nil && err.Field("actual") == float64(c.first); marked != (c.incarnation > 0) {
			t.Fatalf("incarnation %d: lists unchanged since %d: %v", c.incarnation, c.first-1, err)
		}
		if seq := d.write("r2", note, "b"); seq != c.first+1 {
			t.Fatalf("incarnation %d: the second mutation answered %d, want %d", c.incarnation, seq, c.first+1)
		}
		if seq, err := d.guard("lists", c.first); err != nil || seq != c.first+1 {
			t.Fatalf("incarnation %d: lists unchanged since %d: seq %d %v", c.incarnation, c.first, seq, err)
		}
	}
	// No sequence left: a delete is refused too (it needs no _version, so only the sequence stops it).
	d := openDB(t, s, actor.Config{Incarnation: 1 << 21})
	d.seed(ir.NewObject("notes", []any{ir.NewObject("_id", "a", "_creationTime", 0.0, "_version", 1.0, "title", "t", "n", 1.0)}))
	if got := d.exec(body("mutation", "r2", s.Fingerprint, `[{"op":"delete","table":"notes","id":"a"}]`), &listIDs{}, nil); got.err == nil || got.err.Tag != ir.TagStorage {
		t.Fatalf("a delete with no sequence left: %+v", got)
	}
	if docs := d.dump()["notes"].([]any); len(docs) != 1 {
		t.Fatalf("the refused delete left %v", docs)
	}
}
