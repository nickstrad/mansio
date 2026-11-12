// Package owner makes sure at most one process admits writes for a database key at a time.
//
// Object storage lets any process download any database, and routes are allowed to be stale (mansio option 4
// §3.3). So the guarantee lives in the process that hosts the database, in three layers:
//
//  1. An authority (the Placement port; the control plane later) hands out an epoch per (key, owner). It
//     changes the owner only by bumping the epoch, and a move flips only after the source's fence
//     acknowledgement.
//  2. Each process keeps durable local state per key (ACTIVE at epoch E, or FENCED) and admits a request only
//     if its epoch equals the local ACTIVE epoch. After a restart nothing is admitted until the key has been
//     reconciled with the authority. Fence stops admission, drains in-flight work, runs the final write-back
//     and records FENCED durably before it acknowledges.
//  3. Storage is the backstop: every manifest publish carries the epoch and fails with ErrStale if a higher
//     epoch has published (contract 1). A new owner stamps the manifest with its epoch before it admits
//     anything, so a zombie owner's next publish fails and the zombie fences itself.
//
// This file is the pure core: the local states, the transition table (Step), the admission table (decide)
// and the reconcile table (reconcileEvent). No I/O happens here. The safety argument is written out in
// docs/notes/fencing/.
package owner

import "fmt"

// Epoch numbers the authority's ownership grants for one key. Each epoch belongs to exactly one owner; a
// new owner always gets a higher epoch. 0 means "none".
type Epoch uint64

// Generation numbers the immutable snapshots of one key in object storage (contract 1). 0 means "no snapshot
// yet" (a new, empty database).
type Generation uint64

// State is a key's local ownership state in one process.
type State uint8

const (
	// Absent: this process holds nothing for the key.
	Absent State = iota
	// Closed: durably ACTIVE, but not reconciled with the authority since this process started. Admits
	// nothing (option 4 §3.2 "Agent restart: stay closed").
	Closed
	// Active: admits requests at exactly its epoch.
	Active
	// Fencing: admission stopped; the fence is draining or writing back, or its write-back failed and it
	// waits for a retry. Admits nothing. In memory only: durably the key is still ACTIVE, which is safe
	// because a restart comes back Closed.
	Fencing
	// Fenced: durably fenced at its epoch (handed off or superseded). Admits nothing, at any epoch; only a
	// Claim at a higher epoch reopens the key.
	Fenced
)

var stateNames = [...]string{"absent", "closed", "active", "fencing", "fenced"}

func (s State) String() string {
	if int(s) < len(stateNames) {
		return stateNames[s]
	}
	return fmt.Sprintf("state(%d)", s)
}

// Local is one key's local state. Gen is the newest generation this process knows it published or started
// from at Epoch; for Fenced it is the final generation (the one the fence acknowledged).
type Local struct {
	State State
	Epoch Epoch
	Gen   Generation
	// Superseded is set on Fenced when the fence came from a higher epoch (authority or storage), not from
	// Fence. Such a key has no fence acknowledgement: writes admitted at Epoch after Gen may be lost.
	Superseded bool
}

func (l Local) String() string {
	s := fmt.Sprintf("%s(e%d,g%d)", l.State, l.Epoch, l.Gen)
	if l.Superseded {
		s += "!superseded"
	}
	return s
}

// EventKind is what happened to a key.
type EventKind uint8

const (
	// EvClaim: the authority says this process owns the key, ACTIVE at (Epoch, Gen), and storage has been
	// stamped with Epoch.
	EvClaim EventKind = iota
	// EvReopen: reconciliation found the authority ACTIVE with this process as owner at the local epoch.
	EvReopen
	// EvStop: a fence begins; stop admitting.
	EvStop
	// EvFenced: the final write-back published Gen at the local epoch and FENCED is recorded durably.
	EvFenced
	// EvSupersede: a higher epoch exists (the authority says so, or a publish failed with ErrStale).
	EvSupersede
	// EvForget: drop a fenced key's record (the control plane has cleaned the source).
	EvForget
)

var eventNames = [...]string{"claim", "reopen", "stop", "fenced", "supersede", "forget"}

func (k EventKind) String() string {
	if int(k) < len(eventNames) {
		return eventNames[k]
	}
	return fmt.Sprintf("event(%d)", k)
}

// Event is an input to Step. Epoch is used by EvClaim and EvReopen, Gen by EvClaim and EvFenced.
type Event struct {
	Kind  EventKind
	Epoch Epoch
	Gen   Generation
}

// Step is the transition table. It returns the next state, or an error matching one of the sentinels and
// the unchanged state.
//
//	from \ event | claim(E,G)                    | reopen(E)       | stop        | fenced(G)       | supersede  | forget
//	-------------+-------------------------------+-----------------+-------------+-----------------+------------+-------
//	absent       | active(E,G)                   | ErrIllegal      | ErrWrongOwner| ErrIllegal     | absent     | absent
//	closed(e,g)  | ErrUnavailable                | E=e: active     | fencing     | ErrIllegal      | fenced!    | ErrIllegal
//	active(e,g)  | E=e: same; E<e: ErrWrongOwner;| E=e: same       | fencing     | ErrIllegal      | fenced!    | ErrIllegal
//	             |   E>e: ErrIllegal (fence 1st) |                 |             |                 |            |
//	fencing(e,g) | ErrMoving                     | ErrMoving       | same        | G≥g: fenced(e,G)| fenced!    | ErrIllegal
//	fenced(e,g)  | E>e: active(E,G); E=e:        | ErrFenced       | same        | same            | same       | absent
//	             |   ErrFenced; E<e: ErrWrongOwner|                |             |                 |            |
//
// fenced! is fenced with Superseded set. reopen at an epoch other than the local one, and fenced with G < g,
// are ErrIllegal.
//
//	pre:  claim and reopen carry Epoch ≥ 1 (validated at the edge)
//	post: err == nil ⇒ next.Epoch ≥ l.Epoch, or next is absent (forget)
//	post: err != nil ⇒ next == l
func Step(l Local, ev Event) (next Local, err error) {
	illegal := func() (Local, error) {
		return l, fmt.Errorf("%w: %s on %s", ErrIllegal, ev.Kind, l)
	}
	switch ev.Kind {
	case EvClaim:
		switch l.State {
		case Absent:
			return Local{State: Active, Epoch: ev.Epoch, Gen: ev.Gen}, nil
		case Closed:
			return l, ErrUnavailable
		case Active:
			switch {
			case ev.Epoch == l.Epoch:
				return l, nil
			case ev.Epoch < l.Epoch:
				return l, ErrWrongOwner
			default:
				return illegal()
			}
		case Fencing:
			return l, ErrMoving
		case Fenced:
			switch {
			case ev.Epoch > l.Epoch:
				return Local{State: Active, Epoch: ev.Epoch, Gen: ev.Gen}, nil
			case ev.Epoch == l.Epoch:
				return l, ErrFenced
			default:
				return l, ErrWrongOwner
			}
		}
	case EvReopen:
		switch l.State {
		case Closed, Active:
			if ev.Epoch != l.Epoch {
				return illegal()
			}
			return Local{State: Active, Epoch: l.Epoch, Gen: l.Gen}, nil
		case Fencing:
			return l, ErrMoving
		case Fenced:
			return l, ErrFenced
		}
		return illegal()
	case EvStop:
		switch l.State {
		case Absent:
			return l, ErrWrongOwner
		case Closed, Active:
			return Local{State: Fencing, Epoch: l.Epoch, Gen: l.Gen}, nil
		case Fencing, Fenced:
			return l, nil
		}
	case EvFenced:
		switch l.State {
		case Fencing:
			if ev.Gen < l.Gen {
				return illegal()
			}
			return Local{State: Fenced, Epoch: l.Epoch, Gen: ev.Gen}, nil
		case Fenced:
			return l, nil
		}
		return illegal()
	case EvSupersede:
		switch l.State {
		case Absent, Fenced:
			return l, nil
		case Closed, Active, Fencing:
			return Local{State: Fenced, Epoch: l.Epoch, Gen: l.Gen, Superseded: true}, nil
		}
	case EvForget:
		switch l.State {
		case Absent, Fenced:
			return Local{}, nil
		}
		return illegal()
	}
	return illegal()
}

// decide is the admission table (option 4 §3.2), for a request carrying epoch.
//
//	local        | admission
//	-------------+------------------------------------------------------------------
//	absent       | ErrWrongOwner (not held here; re-resolve)
//	closed       | ErrUnavailable (restarted, not reconciled; retry after backoff)
//	active(e)    | epoch = e: admit; otherwise ErrWrongOwner (older or newer ticket)
//	fencing      | ErrMoving (being handed off; re-resolve after backoff), any epoch
//	fenced       | ErrFenced (the local fence overrides even a valid ticket), any epoch
func decide(l Local, epoch Epoch) error {
	switch l.State {
	case Active:
		if epoch == l.Epoch {
			return nil
		}
		return ErrWrongOwner
	case Closed:
		return ErrUnavailable
	case Fencing:
		return ErrMoving
	case Fenced:
		return ErrFenced
	}
	return ErrWrongOwner
}

// reconcileEvent is the reconcile table: what a restarted process does for a key it loaded as Closed, given
// a linearizable read of the authority. ok is false when nothing should change (stay as is).
//
//	authority (found, place)               | event
//	---------------------------------------+---------------------------------------------------------------
//	not found (database dropped)           | supersede → fenced
//	epoch > local                          | supersede → fenced (someone newer owns it)
//	epoch = local, owner ≠ self            | supersede → fenced (authority broke "one owner per epoch"; fail closed)
//	epoch = local, owner = self, ACTIVE    | reopen → active
//	epoch = local, owner = self, MOVING    | stop → fencing (a move was fencing us; wait for Fence to finish it)
//	epoch < local                          | error ErrUnavailable: the authority is behind our durable state
//	                                       |   (a stale read, or a bug); stay closed
//
//	pre: l.State == Closed (other states return ok == false)
func reconcileEvent(l Local, p Place, found bool, self string) (ev Event, ok bool, err error) {
	if l.State != Closed {
		return Event{}, false, nil
	}
	switch {
	case !found, p.Epoch > l.Epoch, p.Epoch == l.Epoch && p.Owner != self:
		return Event{Kind: EvSupersede}, true, nil
	case p.Epoch < l.Epoch:
		return Event{}, false, fmt.Errorf("%w: authority epoch %d is behind local epoch %d", ErrUnavailable, p.Epoch, l.Epoch)
	case p.State == PlaceMoving:
		return Event{Kind: EvStop}, true, nil
	default:
		return Event{Kind: EvReopen, Epoch: l.Epoch}, true, nil
	}
}
