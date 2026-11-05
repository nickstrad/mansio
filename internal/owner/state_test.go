package owner

import (
	"errors"
	"testing"
)

var (
	absent   = Local{}
	closed5  = Local{State: Closed, Epoch: 5, Gen: 3}
	active5  = Local{State: Active, Epoch: 5, Gen: 3}
	fencing5 = Local{State: Fencing, Epoch: 5, Gen: 3}
	fenced5  = Local{State: Fenced, Epoch: 5, Gen: 3}
	sup5     = Local{State: Fenced, Epoch: 5, Gen: 3, Superseded: true}
)

// TestStep is the transition table, one row per cell, with on and off points of every epoch and generation
// comparison (E = e-1, e, e+1; G = g-1, g, g+1).
func TestStep(t *testing.T) {
	claim := func(e Epoch, g Generation) Event { return Event{Kind: EvClaim, Epoch: e, Gen: g} }
	reopen := func(e Epoch) Event { return Event{Kind: EvReopen, Epoch: e} }
	fenced := func(g Generation) Event { return Event{Kind: EvFenced, Gen: g} }
	stop, sup, forget := Event{Kind: EvStop}, Event{Kind: EvSupersede}, Event{Kind: EvForget}
	for _, tc := range []struct {
		from Local
		ev   Event
		want Local
		err  error
	}{
		// absent
		{absent, claim(1, 0), Local{State: Active, Epoch: 1}, nil},
		{absent, reopen(1), absent, ErrIllegal},
		{absent, stop, absent, ErrWrongOwner},
		{absent, fenced(1), absent, ErrIllegal},
		{absent, sup, absent, nil},
		{absent, forget, absent, nil},
		// closed
		{closed5, claim(6, 3), closed5, ErrUnavailable},
		{closed5, reopen(5), active5, nil},
		{closed5, reopen(4), closed5, ErrIllegal},
		{closed5, reopen(6), closed5, ErrIllegal},
		{closed5, stop, fencing5, nil},
		{closed5, fenced(3), closed5, ErrIllegal},
		{closed5, sup, sup5, nil},
		{closed5, forget, closed5, ErrIllegal},
		// active
		{active5, claim(5, 9), active5, nil}, // idempotent: keeps its own gen
		{active5, claim(4, 3), active5, ErrWrongOwner},
		{active5, claim(6, 3), active5, ErrIllegal},
		{active5, reopen(5), active5, nil},
		{active5, reopen(6), active5, ErrIllegal},
		{active5, stop, fencing5, nil},
		{active5, fenced(4), active5, ErrIllegal},
		{active5, sup, sup5, nil},
		{active5, forget, active5, ErrIllegal},
		// fencing
		{fencing5, claim(6, 3), fencing5, ErrMoving},
		{fencing5, reopen(5), fencing5, ErrMoving},
		{fencing5, stop, fencing5, nil},
		{fencing5, fenced(2), fencing5, ErrIllegal}, // off point: G = g-1
		{fencing5, fenced(3), fenced5, nil},         // on point: G = g
		{fencing5, fenced(4), Local{State: Fenced, Epoch: 5, Gen: 4}, nil},
		{fencing5, sup, sup5, nil},
		{fencing5, forget, fencing5, ErrIllegal},
		// fenced
		{fenced5, claim(4, 3), fenced5, ErrWrongOwner},
		{fenced5, claim(5, 3), fenced5, ErrFenced}, // never reopen a fenced epoch
		{fenced5, claim(6, 3), Local{State: Active, Epoch: 6, Gen: 3}, nil},
		{sup5, claim(6, 4), Local{State: Active, Epoch: 6, Gen: 4}, nil},
		{fenced5, reopen(5), fenced5, ErrFenced},
		{fenced5, stop, fenced5, nil},
		{fenced5, fenced(9), fenced5, nil},
		{fenced5, sup, fenced5, nil}, // stays an acknowledged fence
		{fenced5, forget, absent, nil},
		// unknown event
		{active5, Event{Kind: 99}, active5, ErrIllegal},
	} {
		got, err := Step(tc.from, tc.ev)
		if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) || got != tc.want {
			t.Errorf("Step(%s, %s e%d g%d) = %s, %v; want %s, %v", tc.from, tc.ev.Kind, tc.ev.Epoch, tc.ev.Gen, got, err, tc.want, tc.err)
		}
		if err != nil && got != tc.from {
			t.Errorf("Step(%s, %s) changed state on error", tc.from, tc.ev.Kind)
		}
		if err == nil && got.State != Absent && got.Epoch < tc.from.Epoch {
			t.Errorf("Step(%s, %s) moved the epoch back", tc.from, tc.ev.Kind)
		}
	}
}

// TestDecide is the admission table (option 4 §3.2) with on/off points around the local epoch.
func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		l     Local
		epoch Epoch
		want  error
	}{
		{absent, 1, ErrWrongOwner},
		{absent, 0, ErrWrongOwner},
		{closed5, 5, ErrUnavailable},
		{active5, 4, ErrWrongOwner}, // off point: older ticket
		{active5, 5, nil},           // on point
		{active5, 6, ErrWrongOwner}, // off point: newer ticket (not claimed here yet)
		{active5, 0, ErrWrongOwner},
		{fencing5, 5, ErrMoving},
		{fencing5, 6, ErrMoving},
		{fenced5, 5, ErrFenced},
		{fenced5, 6, ErrFenced}, // the local fence overrides a valid ticket
		{sup5, 5, ErrFenced},
	} {
		if got := decide(tc.l, tc.epoch); got != tc.want {
			t.Errorf("decide(%s, %d) = %v, want %v", tc.l, tc.epoch, got, tc.want)
		}
	}
}

func TestReconcileEvent(t *testing.T) {
	act := func(owner string, e Epoch) Place { return Place{Owner: owner, Epoch: e, Gen: 3, State: PlaceActive} }
	mov := func(owner string, e Epoch) Place { return Place{Owner: owner, Epoch: e, Gen: 3, State: PlaceMoving} }
	for _, tc := range []struct {
		name  string
		l     Local
		p     Place
		found bool
		want  EventKind
		ok    bool
		err   error
	}{
		{"dropped", closed5, Place{}, false, EvSupersede, true, nil},
		{"newer epoch elsewhere", closed5, act("b", 6), true, EvSupersede, true, nil},
		{"newer epoch for self", closed5, act("a", 6), true, EvSupersede, true, nil},
		{"same epoch other owner", closed5, act("b", 5), true, EvSupersede, true, nil},
		{"same epoch self active", closed5, act("a", 5), true, EvReopen, true, nil},
		{"same epoch self moving", closed5, mov("a", 5), true, EvStop, true, nil},
		{"authority behind", closed5, act("a", 4), true, 0, false, ErrUnavailable},
		{"not closed", active5, act("b", 9), true, 0, false, nil},
		{"fenced", fenced5, act("a", 5), true, 0, false, nil},
	} {
		ev, ok, err := reconcileEvent(tc.l, tc.p, tc.found, "a")
		if ok != tc.ok || !errors.Is(err, tc.err) || (tc.err == nil && err != nil) || (ok && ev.Kind != tc.want) {
			t.Errorf("%s: reconcileEvent = %s %v %v; want %s %v %v", tc.name, ev.Kind, ok, err, tc.want, tc.ok, tc.err)
		}
		if ok {
			if _, err := Step(tc.l, ev); err != nil {
				t.Errorf("%s: reconcile event %s is not legal on %s: %v", tc.name, ev.Kind, tc.l, err)
			}
		}
	}
}

func TestStrings(t *testing.T) {
	if s := sup5.String(); s != "fenced(e5,g3)!superseded" {
		t.Errorf("String = %q", s)
	}
	if State(9).String() != "state(9)" || EventKind(9).String() != "event(9)" || PlaceState(0).String() != "?" {
		t.Error("unknown values must print their number")
	}
}
