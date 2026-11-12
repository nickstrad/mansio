package owner

import (
	"context"
	"errors"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// Agent is what the control plane calls on a process during a move. *Table implements it; over the network
// it would be the agent's lifecycle listener (agentd's /v1/lifecycle).
type Agent interface {
	Fence(ctx context.Context, key string) (Ack, error)
	Claim(ctx context.Context, key string) (Local, error)
}

// Mover runs the offline move protocol as the control plane would:
//
//  1. CAS ACTIVE(src, E) → MOVING(src, E)       (clients of src now get ErrMoving/ErrFenced)
//  2. src.Fence → Ack{E, G}                    (stop, drain, final write-back of G at E, durable FENCED)
//  3. CAS MOVING(src, E) → ACTIVE(dst, E+1, G)  (the flip; only with the source's Ack)
//  4. dst.Claim                                (reads the flip, stamps E+1 in storage, durable ACTIVE)
//
// Move is resumable: each call reads the authority and continues from the step it finds, so after any crash
// or error the caller just calls Move again. Request-id de-duplication is not here: it lives
// in the database (the request-id table copied with the snapshot), so a client retry after an ambiguous
// failure is answered by whichever owner the retry reaches.
type Mover struct {
	Placement Placement
	Agent     func(owner string) Agent
}

type moveAction uint8

const (
	moveBegin moveAction = iota + 1
	moveFenceAndFlip
	moveClaim
)

// nextMove is the pure decision: given the authority's row and the target, what to do next.
//
//	row                    | action
//	-----------------------+--------------------------------------------------
//	ACTIVE, owner = dst    | claim dst (finish, or nothing to move)
//	ACTIVE, owner ≠ dst    | begin: CAS to MOVING
//	MOVING                 | fence the row's owner, then flip to dst at epoch+1
func nextMove(p Place, dst string) moveAction {
	switch {
	case p.State == PlaceMoving:
		return moveFenceAndFlip
	case p.Owner == dst:
		return moveClaim
	default:
		return moveBegin
	}
}

// Move makes dst the active owner of key.
//
//	post: err == nil ⇒ the authority says ACTIVE(dst, E'), dst is Active at E' and storage carries E'; every
//	      write the source admitted before its fence is in the generation dst started from
//	post: err != nil ⇒ the move stopped at some step; call Move again to resume
func (m *Mover) Move(ctx context.Context, key, dst string) (Local, error) {
	for range 4 {
		p, err := m.Placement.Get(ctx, key)
		if err != nil {
			return Local{}, errs.Wrap(module, err, "cannot move %q: authority unreadable; resume later", key)
		}
		switch nextMove(p, dst) {
		case moveClaim:
			l, err := m.Agent(dst).Claim(ctx, key)
			if err != nil {
				return Local{}, errs.Wrap(module, err, "cannot move %q: target %q did not claim it; resume later", key, dst)
			}
			return l, nil
		case moveBegin:
			next := p
			next.State = PlaceMoving
			if err := m.Placement.CAS(ctx, key, p, next); err != nil {
				return Local{}, errs.Wrap(module, err, "cannot move %q: begin (ACTIVE→MOVING) failed; resume later", key)
			}
		case moveFenceAndFlip:
			ack, err := m.Agent(p.Owner).Fence(ctx, key)
			if err != nil {
				return Local{}, errs.Wrap(module, err, "cannot move %q: source %q did not acknowledge the fence; still MOVING, resume later", key, p.Owner)
			}
			if ack.Epoch != p.Epoch || ack.Gen < p.Gen {
				return Local{}, errs.Wrap(module, ErrInvalid, "cannot move %q: fence ack %+v does not match row %+v; not flipping", key, ack, p)
			}
			next := Place{Owner: dst, Epoch: p.Epoch + 1, Gen: ack.Gen, State: PlaceActive}
			if err := m.Placement.CAS(ctx, key, p, next); err != nil && !errors.Is(err, ErrConflict) {
				return Local{}, errs.Wrap(module, err, "cannot move %q: flip failed or its reply was lost; resume later", key)
			}
			// On ErrConflict someone else changed the row; the next round reads it again.
		}
	}
	return Local{}, errs.Wrap(module, ErrConflict, "cannot move %q: the row kept changing; resume later", key)
}
