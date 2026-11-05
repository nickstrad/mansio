package pg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nickstrad/ts-db-go/internal/breaker"
	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/owner"
)

// Placement is owner.Placement over Postgres. Safe for concurrent use. Every method reads and writes the
// primary the DSN names.
type Placement struct {
	cfg  Config
	pool *pgxpool.Pool
	b    *breaker.Breaker
}

var _ owner.Placement = (*Placement)(nil)

// Open validates cfg and builds the pool. It does not connect: the first call does, so a process starts
// even while the authority is down (RI H1). Call Migrate before serving; Close when done.
func Open(cfg Config) (*Placement, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	pc, err := cfg.poolConfig()
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc) // no I/O with MinConns 0
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot create the placement pool; nothing connected")
	}
	b, err := breaker.New(cfg.Breaker)
	if err != nil {
		pool.Close()
		return nil, errs.Wrap(module, err, "invalid placement breaker config")
	}
	return &Placement{cfg: cfg, pool: pool, b: b}, nil
}

// Close closes the pool, waiting for checked-out connections to return.
func (p *Placement) Close() { p.pool.Close() }

// Breaker is the pool's circuit breaker, for metrics and runtime controls (register it as "placement").
func (p *Placement) Breaker() *breaker.Breaker { return p.b }

// Stat is the pool's counters (acquires, waits, idle, total).
func (p *Placement) Stat() *pgxpool.Stat { return p.pool.Stat() }

// call runs fn once under the breaker and a per-call deadline (RI T1, C1). what names the call for the
// error; retry says whether a retry is safe after a system failure.
func (p *Placement) call(ctx context.Context, what, retry string, fn func(ctx context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return errs.Wrap(module, err, "cannot %s: the caller gave up; nothing sent", what)
	}
	permit, wait, ok := p.b.Allow(p.cfg.Now())
	if !ok {
		return errs.Wrap(module, &openError{wait: wait}, "cannot %s: the placement circuit breaker is open; nothing sent, retry after %v", what, wait)
	}
	cctx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
	err := fn(cctx)
	cancel()
	if err != nil && !isAnswer(err) {
		err = classify(err)
	}
	o := breaker.Success
	switch {
	case err == nil || isAnswer(err):
	case gaveUp(ctx):
		o = breaker.Ignore // the caller left: says nothing about the authority
	default:
		o = breaker.Failure
	}
	p.b.Done(permit, p.cfg.Now(), o)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnavailable):
		return errs.Wrap(module, err, "cannot %s: placement authority unavailable; %s", what, retry)
	}
	return errs.Wrap(module, err, "cannot %s", what)
}

// Get returns key's row, read from the primary. A missing key matches owner.ErrNotFound.
func (p *Placement) Get(ctx context.Context, key string) (owner.Place, error) {
	if err := checkKey(key); err != nil {
		return owner.Place{}, errs.Wrap(module, err, "cannot read the placement of %q; nothing sent", key)
	}
	var pl owner.Place
	err := p.call(ctx, fmt.Sprintf("read the placement of %q", key), "nothing changed, safe to retry", func(ctx context.Context) error {
		var vm, state string
		var epoch, gen int64
		err := p.pool.QueryRow(ctx, q.get, key).Scan(&vm, &epoch, &gen, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return owner.ErrNotFound
		}
		if err != nil {
			return err
		}
		pl, err = decodePlace(vm, epoch, gen, state)
		return err
	})
	return pl, err
}

// CAS sets key's row to next iff it equals expect (all four fields); expect.Epoch == 0 means "no row" and
// creates it. A flip (MOVING on one VM → ACTIVE on another) records its moves row in the same statement.
//
//	post: nil ⇒ the row is next
//	post: owner.ErrConflict ⇒ the row was not expect; nothing changed
//	post: ErrInvariant, ErrUnknownVM, owner.ErrInvalid ⇒ refused; nothing changed
//	post: ErrUnavailable ⇒ unknown: the change may have applied; Get before deciding
func (p *Placement) CAS(ctx context.Context, key string, expect, next owner.Place) error {
	what := fmt.Sprintf("set the placement of %q", key)
	if err := checkKey(key); err != nil {
		return errs.Wrap(module, err, "cannot %s; nothing sent", what)
	}
	if err := checkPlace(next); err != nil {
		return errs.Wrap(module, err, "cannot %s: bad next row; nothing sent", what)
	}
	if expect.Epoch == 0 {
		if expect != (owner.Place{}) {
			return errs.Wrap(module, owner.ErrConflict, "cannot %s: expected \"no row\" with fields set: %+v", what, expect)
		}
		return p.call(ctx, what, "it may have been created: Get before retrying", func(ctx context.Context) error {
			var e int64
			err := p.pool.QueryRow(ctx, q.create, key, next.Owner, int64(next.Epoch), int64(next.Gen), next.State.String()).Scan(&e)
			if errors.Is(err, pgx.ErrNoRows) {
				return owner.ErrConflict // the key exists
			}
			return err
		})
	}
	if checkPlace(expect) != nil {
		// No stored row can equal it (the schema refuses such rows), so the row is not the expected one.
		return errs.Wrap(module, owner.ErrConflict, "cannot %s: expected row %+v cannot exist", what, expect)
	}
	return p.call(ctx, what, "it may have applied: Get before retrying", func(ctx context.Context) error {
		var n int64
		err := p.pool.QueryRow(ctx, q.cas, key,
			expect.Owner, int64(expect.Epoch), int64(expect.Gen), expect.State.String(),
			next.Owner, int64(next.Epoch), int64(next.Gen), next.State.String()).Scan(&n)
		if err != nil {
			return err
		}
		if n == 0 {
			return owner.ErrConflict
		}
		return nil
	})
}

// RegisterVM adds vm to the registry (or refreshes its endpoint and heartbeat); a row may name it as owner
// only after this. Idempotent.
func (p *Placement) RegisterVM(ctx context.Context, vm, endpoint string) error {
	what := fmt.Sprintf("register vm %q", vm)
	if !ValidVM(vm) {
		return errs.Wrap(module, owner.ErrInvalid, "cannot %s: a vm id is 1-63 of [a-z0-9.-], starting with [a-z0-9]", what)
	}
	return p.call(ctx, what, "safe to retry", func(ctx context.Context) error {
		_, err := p.pool.Exec(ctx, q.registerVM, vm, endpoint)
		return err
	})
}

// Stuck is a row that has been MOVING for a while: a move to resume.
type Stuck struct {
	Key   string
	Owner string
	Epoch owner.Epoch
	Gen   owner.Generation
}

// Moving returns up to limit rows MOVING for longer than olderThan, oldest first (moving.sql).
func (p *Placement) Moving(ctx context.Context, olderThan time.Duration, limit int) ([]Stuck, error) {
	var out []Stuck
	err := p.call(ctx, "list stuck moves", "nothing changed, safe to retry", func(ctx context.Context) error {
		out = out[:0]
		rows, err := p.pool.Query(ctx, q.moving, olderThan, limit)
		if err != nil {
			return err
		}
		var s Stuck
		var e, g int64
		_, err = pgx.ForEachRow(rows, []any{&s.Key, &s.Owner, &e, &g}, func() error {
			s.Epoch, s.Gen = owner.Epoch(e), owner.Generation(g)
			out = append(out, s)
			return nil
		})
		return err
	})
	return out, err
}

// VMLoad is one live VM with its count of ACTIVE databases.
type VMLoad struct {
	VM        string
	Endpoint  string
	ActiveDBs int64
}

// Load returns up to limit live VMs, least loaded first (vm_load.sql).
func (p *Placement) Load(ctx context.Context, limit int) ([]VMLoad, error) {
	var out []VMLoad
	err := p.call(ctx, "read vm load", "nothing changed, safe to retry", func(ctx context.Context) error {
		out = out[:0]
		rows, err := p.pool.Query(ctx, q.vmLoad, limit)
		if err != nil {
			return err
		}
		var l VMLoad
		_, err = pgx.ForEachRow(rows, []any{&l.VM, &l.Endpoint, &l.ActiveDBs}, func() error {
			out = append(out, l)
			return nil
		})
		return err
	})
	return out, err
}

// Violation is a moves row whose placement row is missing (Epoch 0) or not above the flip's from_epoch.
type Violation struct {
	Key       string
	FromEpoch owner.Epoch
	Epoch     owner.Epoch
}

// Invariants returns up to limit violations of the move invariant (invariants.sql); none when consistent.
func (p *Placement) Invariants(ctx context.Context, limit int) ([]Violation, error) {
	var out []Violation
	err := p.call(ctx, "check placement invariants", "nothing changed, safe to retry", func(ctx context.Context) error {
		out = out[:0]
		rows, err := p.pool.Query(ctx, q.invariants, limit)
		if err != nil {
			return err
		}
		var v Violation
		var from int64
		var epoch *int64
		_, err = pgx.ForEachRow(rows, []any{&v.Key, &from, &epoch}, func() error {
			v.FromEpoch, v.Epoch = owner.Epoch(from), 0
			if epoch != nil {
				v.Epoch = owner.Epoch(*epoch)
			}
			out = append(out, v)
			return nil
		})
		return err
	})
	return out, err
}

var vmID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`) // the vm_id domain

// ValidVM reports whether vm is a valid VM id (the schema's vm_id domain).
func ValidVM(vm string) bool { return vmID.MatchString(vm) }

// checkKey validates a key at the edge: owner.ValidKey, and text Postgres can store (valid UTF-8, no NUL).
func checkKey(key string) error {
	if !owner.ValidKey(key) || !utf8.ValidString(key) || containsNUL(key) {
		return fmt.Errorf("%w: key %q must be 1-%d bytes of UTF-8 without NUL", owner.ErrInvalid, key, owner.MaxKeyLen)
	}
	return nil
}

func containsNUL(s string) bool {
	for i := range len(s) {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

// checkPlace validates a row the caller wants stored: a valid VM, epoch 1..MaxInt64, generation
// 0..MaxInt64, a known state. These are the schema's domains; checking them here sends nothing doomed.
func checkPlace(pl owner.Place) error {
	switch {
	case !ValidVM(pl.Owner):
		return fmt.Errorf("%w: invalid owner vm %q", owner.ErrInvalid, pl.Owner)
	case pl.Epoch == 0 || pl.Epoch > math.MaxInt64:
		return fmt.Errorf("%w: invalid epoch %d: must be 1..2^63-1", owner.ErrInvalid, pl.Epoch)
	case pl.Gen > math.MaxInt64:
		return fmt.Errorf("%w: invalid generation %d: must be ≤ 2^63-1", owner.ErrInvalid, pl.Gen)
	case pl.State != owner.PlaceActive && pl.State != owner.PlaceMoving:
		return fmt.Errorf("%w: invalid state %d", owner.ErrInvalid, pl.State)
	}
	return nil
}

// decodePlace maps a stored row; the schema's domains make every branch but the last unreachable.
func decodePlace(vm string, epoch, gen int64, state string) (owner.Place, error) {
	pl := owner.Place{Owner: vm, Epoch: owner.Epoch(epoch), Gen: owner.Generation(gen)}
	switch state {
	case "ACTIVE":
		pl.State = owner.PlaceActive
	case "MOVING":
		pl.State = owner.PlaceMoving
	default:
		return owner.Place{}, fmt.Errorf("%w: stored state %q is not ACTIVE or MOVING; is the schema newer than this binary?", ErrInvariant, state)
	}
	return pl, nil
}
