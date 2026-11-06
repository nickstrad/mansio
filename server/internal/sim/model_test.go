package sim

import (
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/front"
)

var mt0 = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return mt0.Add(time.Duration(s * float64(time.Second))) }

func newTestModel(rpo time.Duration) (*model, *health) {
	h := newHealth()
	return newModel(mt0, rpo, h, newTrace(mt0, func() time.Time { return mt0 }, nil), []string{"k"}), h
}

// write sends id and has the agent answer a.
func write(m *model, t float64, id string, a Answer) {
	m.sent("k", id)
	m.answer(at(t), "k", Request{Kind: Write, ID: id}, a, true)
}

var ok = Answer{Code: front.OK}

func wantViolation(t *testing.T, m *model, substr string) {
	t.Helper()
	for _, v := range m.violations() {
		if strings.Contains(v.What, substr) {
			return
		}
	}
	t.Fatalf("no violation containing %q; got %v", substr, m.violations())
}

func wantNone(t *testing.T, m *model) {
	t.Helper()
	if v := m.violations(); len(v) > 0 {
		t.Fatalf("violations: %v", v)
	}
}

func TestModelAcceptsAConsistentHistory(t *testing.T) {
	m, _ := newTestModel(10 * time.Second)
	write(m, 1, "a", ok)
	write(m, 2, "b", Answer{Code: front.Expired, Ambiguous: true}) // maybe
	write(m, 2.5, "b", Answer{Code: front.OK, Duplicate: true})    // it had applied
	write(m, 3, "c", Answer{Code: front.Unavailable})              // nothing ran
	m.answer(at(4), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"a", "b"}}, true)
	m.generation(at(5), "k", durable.Manifest{Generation: 2, Epoch: 1}, func() ([]string, error) { return []string{"a"}, nil })
	m.durable("k", at(5))
	m.restore(at(6), "k", []string{"a", "b"})
	write(m, 7, "c", ok) // c finally applies, after the restore
	m.answer(at(8), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"a", "b", "c"}}, true)
	wantNone(t, m)
}

// Each property, with one history that breaks it.
func TestModelFindsEachViolation(t *testing.T) {
	cases := []struct {
		name, want string
		run        func(m *model, h *health)
	}{
		{"second ok without duplicate", "applied twice", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			write(m, 2, "a", ok)
		}},
		{"mark twice in a read", "applied twice", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			m.answer(at(2), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"a", "a"}}, true)
		}},
		{"duplicate of a write that never ran", "never applied", func(m *model, _ *health) {
			write(m, 1, "a", Answer{Code: front.Shed})
			write(m, 2, "a", Answer{Code: front.OK, Duplicate: true})
		}},
		{"applied though nothing ran", "every answer said nothing ran", func(m *model, _ *health) {
			write(m, 1, "a", Answer{Code: front.StmtTimeout})
			m.answer(at(2), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"a"}}, true)
		}},
		{"rejected write acknowledged", "rejects", func(m *model, _ *health) {
			m.sent("k", "a")
			m.answer(at(1), "k", Request{Kind: BadWrite, ID: "a"}, ok, true)
		}},
		{"acknowledged write missing from a live read", "missing", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			write(m, 2, "b", ok)
			m.answer(at(3), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"b"}}, true)
		}},
		{"out of order", "out of commit order", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			write(m, 2, "b", ok)
			m.answer(at(3), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"b", "a"}}, true)
		}},
		{"unknown mark", "unknown mark", func(m *model, _ *health) {
			m.answer(at(1), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"zz"}}, true)
		}},
		{"generation goes back", "back to generation", func(m *model, _ *health) {
			m.generation(at(1), "k", durable.Manifest{Generation: 3, Epoch: 2}, func() ([]string, error) { return nil, nil })
			m.generation(at(2), "k", durable.Manifest{Generation: 3, Epoch: 1}, nil)
		}},
		{"generation does not extend the last", "does not extend", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			write(m, 2, "b", ok)
			m.generation(at(3), "k", durable.Manifest{Generation: 2, Epoch: 1}, func() ([]string, error) { return []string{"a", "b"}, nil })
			m.generation(at(4), "k", durable.Manifest{Generation: 3, Epoch: 1}, func() ([]string, error) { return []string{"b"}, nil })
		}},
		{"hole in a generation", "missing", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			write(m, 2, "b", ok)
			m.generation(at(3), "k", durable.Manifest{Generation: 2, Epoch: 1}, func() ([]string, error) { return []string{"b"}, nil })
		}},
		{"lost after a write-back that published", "before a write-back that published", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			m.durable("k", at(2))
			m.restore(at(3), "k", nil)
		}},
		{"lost after a fault-free RPO", "fault-free", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			m.restore(at(12), "k", nil)
		}},
		{"lost write reappears", "not in the history since the last restore", func(m *model, _ *health) {
			write(m, 1, "a", ok)
			m.restore(at(2), "k", nil)
			m.answer(at(3), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"a"}}, true)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, h := newTestModel(10 * time.Second)
			c.run(m, h)
			wantViolation(t, m, c.want)
		})
	}
}

// A loss inside the RPO, or with faults covering the window, is allowed: on and off points of the RPO.
func TestModelRPOWindow(t *testing.T) {
	cases := []struct {
		name  string
		crash float64
		spans [][2]float64
		bad   bool
	}{
		{"just inside the RPO", 10.9, nil, false},
		{"exactly the RPO", 11, nil, true},
		{"faults split the window", 30, [][2]float64{{9, 9}, {18, 20}, {29, 29}}, false},
		{"one fault-free stretch is long enough", 30, [][2]float64{{9, 9}}, true},
		{"an open span covers the rest", 30, [][2]float64{{5, -1}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, h := newTestModel(10 * time.Second)
			write(m, 1, "a", ok)
			for _, s := range c.spans {
				if s[1] < 0 {
					h.begin("storm", at(s[0]))
					continue
				}
				h.mark(at(s[0]), at(s[1]))
			}
			m.restore(at(c.crash), "k", nil)
			if got := len(m.violations()) > 0; got != c.bad {
				t.Fatalf("violation = %v, want %v (%v); allowed losses %d", got, c.bad, m.violations(), m.lostOK)
			}
		})
	}
}

// A write lost at a crash may be sent again with the same id and apply once more; a dead process's answer
// leaves the write open.
func TestModelRetryAfterLossAndDeadAnswers(t *testing.T) {
	m, _ := newTestModel(10 * time.Second)
	write(m, 1, "a", ok)
	m.sent("k", "b")
	m.answer(at(2), "k", Request{Kind: Write, ID: "b"}, ok, false) // the process died before it could answer
	m.restore(at(3), "k", nil)                                     // both lost, inside the RPO
	write(m, 4, "a", ok)
	write(m, 5, "b", ok)
	m.answer(at(6), "k", Request{Kind: Read}, Answer{Code: front.OK, Marks: []string{"a", "b"}}, true)
	wantNone(t, m)
	if m.lostOK != 1 {
		t.Fatalf("allowed losses %d, want 1 (only a was acknowledged)", m.lostOK)
	}
}
