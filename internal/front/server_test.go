package front_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/assert"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
	"github.com/nickstrad/ts-db-go/internal/ticket"
	"github.com/nickstrad/ts-db-go/internal/ticket/tickettest"
)

const vm = "vm-1"

type rig struct {
	s     *front.Server
	adm   *fronttest.Admitter
	ex    *fronttest.Executor
	iss   tickettest.Issuer
	ring  *ticket.KeyRing
	clock *fronttest.Clock
	tv    front.TicketVerifier
}

func newRig(t testing.TB, cfg front.Config) *rig {
	iss := tickettest.NewIssuer("k1")
	ring := ticket.NewKeyRing(iss.Keys())
	clock := fronttest.NewClock(time.Now())
	r := &rig{adm: fronttest.NewAdmitter(map[string]uint64{"db-1": 1, "db-2": 1}), ex: fronttest.NewExecutor("db-1"),
		iss: iss, ring: ring, clock: clock}
	v := ticket.Verifier{Keys: ring, VM: vm, Skew: 5 * time.Second, MaxTTL: time.Hour}
	r.tv = ticket.NewCache(v, 128)
	r.s = front.NewServer(cfg, r.tv, r.adm, r.ex, clock.Now)
	return r
}

func (r *rig) tok(t testing.TB, db string, mut func(*ticket.Claims)) string {
	c := r.iss.Claims(db, vm, r.clock.Now(), time.Minute)
	if mut != nil {
		mut(&c)
	}
	return r.iss.Mint(t, c)
}

func body(db string, kind front.Kind, sql string, args ...any) []byte {
	b, _ := json.Marshal(map[string]any{"db": db, "request_id": "r1", "kind": kind, "sql": sql, "args": args})
	return b
}

// balanced checks that every admission, slot and in-flight count was released.
func (r *rig) balanced(t *testing.T) {
	t.Helper()
	if _, open, doubles := r.adm.Counts(); open != 0 || doubles != 0 {
		t.Errorf("admitter: %d unreleased, %d double releases", open, doubles)
	}
	if s := r.s.Admission().Stats(); s != (front.AdmissionStats{}) {
		t.Errorf("admission state left: %+v", s)
	}
	if n := r.s.InFlight(); n != 0 {
		t.Errorf("in flight %d", n)
	}
}

var (
	get = func(k int64) []byte { return body("db-1", front.KindRead, fronttest.GetSQL, k) }
	put = func(k int64) []byte { return body("db-1", front.KindWrite, fronttest.PutSQL, k, "v") }
)

// Every wire code through the whole pipeline, with the fakes' fault switches.
func TestPipelineWireCodes(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, r *rig) front.Input
		code   front.Code
		status int
		check  func(t *testing.T, o front.Output)
	}{
		{"ok read", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		},
			front.OK, 200, func(t *testing.T, o front.Output) {
				if o.Resp.Count != 1 || len(o.Resp.Rows) != 1 || o.Resp.RequestID != "r1" || o.Resp.Timing == nil {
					t.Errorf("%+v", o.Resp)
				}
			}},
		{"call carries the ticket's tenant, epoch and gen", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-1", func(c *ticket.Claims) { c.Epoch, c.Gen = 1, 9 }), Body: get(1)}
		}, front.OK, 200, nil},
		{"ok write", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: put(9)}
		},
			front.OK, 200, func(t *testing.T, o front.Output) {
				if o.Resp.Count != 1 {
					t.Errorf("%+v", o.Resp)
				}
			}},
		{"shed by executor", func(t *testing.T, r *rig) front.Input {
			r.ex.Set(func(e *fronttest.Executor) { e.Err = front.ErrShed })
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		}, front.Shed, 503, func(t *testing.T, o front.Output) {
			if o.RetryAfter != 1 || o.Resp.RetryAfterMS != 1000 {
				t.Errorf("retry after %d / %d", o.RetryAfter, o.Resp.RetryAfterMS)
			}
		}},
		{"expired in executor", func(t *testing.T, r *rig) front.Input {
			r.ex.Set(func(e *fronttest.Executor) { e.Err = front.ErrExpired })
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: put(9)}
		}, front.Expired, 504, func(t *testing.T, o front.Output) {
			if o.Resp.Ambiguous {
				t.Error("a write that never ran is not ambiguous")
			}
		}},
		{"no reply, write: ambiguous", func(t *testing.T, r *rig) front.Input {
			r.ex.Set(func(e *fronttest.Executor) { e.Block = true })
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: put(9), Timeout: 20 * time.Millisecond}
		}, front.Expired, 504, func(t *testing.T, o front.Output) {
			if !o.Resp.Ambiguous {
				t.Error("write without a reply must be ambiguous")
			}
		}},
		{"no reply, read: not ambiguous", func(t *testing.T, r *rig) front.Input {
			r.ex.Set(func(e *fronttest.Executor) { e.Block = true })
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1), Timeout: 20 * time.Millisecond}
		}, front.Expired, 504, func(t *testing.T, o front.Output) {
			if o.Resp.Ambiguous {
				t.Error("read marked ambiguous")
			}
		}},
		{"stmt timeout", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: body("db-1", front.KindRead, fronttest.SlowSQL)}
		}, front.StmtTimeout, 422, nil},
		{"aborted", func(t *testing.T, r *rig) front.Input {
			r.ex.Set(func(e *fronttest.Executor) { e.Err = front.ErrAborted })
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: put(9)}
		}, front.Aborted, 409, nil},
		{"wrong owner: ticket epoch is old", func(t *testing.T, r *rig) front.Input {
			r.adm.Set("db-1", 2)
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		}, front.WrongOwner, 421, nil},
		{"wrong owner: db not hosted", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-2", nil), Body: body("db-2", front.KindRead, fronttest.GetSQL, 1)}
		}, front.WrongOwner, 421, nil},
		{"fenced", func(t *testing.T, r *rig) front.Input {
			r.adm.SetErr(front.ErrFenced)
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		}, front.Fenced, 421, func(t *testing.T, o front.Output) {
			if o.RetryAfter != 1 {
				t.Errorf("retry after %d", o.RetryAfter)
			}
		}},
		{"ticket missing", func(t *testing.T, r *rig) front.Input { return front.Input{Body: get(1)} }, front.TicketInvalid, 401, nil},
		{"ticket expired", func(t *testing.T, r *rig) front.Input {
			tok := r.tok(t, "db-1", nil)
			r.clock.Advance(time.Minute + 5*time.Second)
			return front.Input{Ticket: tok, Body: get(1)}
		}, front.TicketInvalid, 401, nil},
		{"ticket for another db", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-2", nil), Body: get(1)}
		}, front.TicketInvalid, 401, nil},
		{"read-only ticket, write", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-1", func(c *ticket.Claims) { c.Perms = ticket.Read }), Body: put(9)}
		}, front.TicketInvalid, 401, func(t *testing.T, o front.Output) {
			if !errors.Is(o.Err, ticket.ErrPermission) {
				t.Errorf("cause %v", o.Err)
			}
		}},
		{"ticket key rotated away mid-flight", func(t *testing.T, r *rig) front.Input {
			tok := r.tok(t, "db-1", nil)
			r.ring.Replace(tickettest.NewIssuer("k2").Keys())
			return front.Input{Ticket: tok, Body: get(1)}
		}, front.TicketInvalid, 401, nil},
		{"bad body", func(t *testing.T, r *rig) front.Input { return front.Input{Ticket: "x", Body: []byte(`{`)} }, front.BadRequest, 400,
			func(t *testing.T, o front.Output) {
				if !strings.Contains(o.Resp.Message, "not a valid request") {
					t.Errorf("message %q", o.Resp.Message)
				}
			}},
		{"too many rows", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: body("db-1", front.KindRead, fronttest.ScanSQL)}
		}, front.BadRequest, 400, nil},
		{"statement rejected", func(t *testing.T, r *rig) front.Input {
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: put(1)} // duplicate key
		}, front.BadRequest, 400, func(t *testing.T, o front.Output) {
			if strings.Contains(o.Resp.Message, "UNIQUE") {
				t.Errorf("driver text leaked: %q", o.Resp.Message)
			}
		}},
		{"internal: raw executor error", func(t *testing.T, r *rig) front.Input {
			r.ex.Set(func(e *fronttest.Executor) { e.Err = errors.New("disk on fire") })
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		}, front.Internal, 500, func(t *testing.T, o front.Output) {
			if strings.Contains(o.Resp.Message, "fire") {
				t.Errorf("internal text leaked: %q", o.Resp.Message)
			}
		}},
		{"internal: executor panics", func(t *testing.T, r *rig) front.Input {
			r.ex.Set(func(e *fronttest.Executor) { e.Panic = true })
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		}, front.Internal, 500, func(t *testing.T, o front.Output) {
			if o.Stack == "" || !strings.Contains(o.Err.Error(), "on purpose") {
				t.Errorf("panic not carried for the log: %v", o.Err)
			}
		}},
		{"internal: admitter panics", func(t *testing.T, r *rig) front.Input {
			r.adm.Panic = true
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		}, front.Internal, 500, nil},
		{"internal: admitter error of unknown kind", func(t *testing.T, r *rig) front.Input {
			r.adm.SetErr(errors.New("owner store unreadable"))
			return front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}
		}, front.Internal, 500, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, front.Config{Limits: front.Limits{MaxRows: 2}, StmtTimeout: 20 * time.Millisecond})
			out := r.s.Handle(context.Background(), tc.setup(t, r))
			if out.Resp.Code != tc.code || out.Status != tc.status {
				t.Fatalf("code %s status %d (err %v), want %s %d", out.Resp.Code, out.Status, out.Err, tc.code, tc.status)
			}
			if tc.code != front.OK && out.Resp.Message == "" {
				t.Error("no message")
			}
			if tc.check != nil {
				tc.check(t, out)
			}
			if c := r.ex.Last(); tc.code == front.OK && (c.Tenant != "tenant-1" || c.Epoch != 1 || (c.Gen != 1 && c.Gen != 9) || c.Deadline.IsZero() || c.StmtTimeout <= 0) {
				t.Errorf("call %+v", c)
			}
			r.balanced(t)
		})
	}
}

// A broken Admitter adapter (nil release, no error) is a bug: the checked call asserts, the pipeline answers
// internal.
func TestPipelineAssertsOnABrokenAdmitter(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out")
	}
	r := newRig(t, front.Config{})
	r.adm.NoRel = true
	if out := r.s.Handle(context.Background(), front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1)}); out.Resp.Code != front.Internal ||
		!strings.Contains(out.Err.Error(), "no release") {
		t.Fatalf("%s %v", out.Resp.Code, out.Err)
	}
}

// A broken Executor (more rows than MaxRows) trips the contract assertion.
func TestPipelineAssertsOnABrokenExecutor(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out")
	}
	iss := tickettest.NewIssuer("k1")
	v := ticket.Verifier{Keys: ticket.NewKeyRing(iss.Keys()), VM: vm, Skew: time.Second}
	s := front.NewServer(front.Config{Limits: front.Limits{MaxRows: 1}}, v,
		fronttest.NewAdmitter(map[string]uint64{"db-1": 1}), brokenExec{}, nil)
	tok := iss.Mint(t, iss.Claims("db-1", vm, time.Now(), time.Minute))
	if out := s.Handle(context.Background(), front.Input{Ticket: tok, Body: get(1)}); out.Resp.Code != front.Internal ||
		!strings.Contains(out.Err.Error(), "contract") {
		t.Fatalf("%s %v", out.Resp.Code, out.Err)
	}
}

type brokenExec struct{}

func (brokenExec) Exec(context.Context, front.Call) (front.Reply, error) {
	return front.Reply{Rows: [][]any{{1}, {2}}, Count: 2}, nil
}

// Admission through the pipeline: one slot per database, a second request waits, a third is shed; the waiter
// that outlives its deadline expires.
func TestPipelineAdmissionShedsAndExpires(t *testing.T) {
	r := newRig(t, front.Config{Admission: front.AdmissionLimits{TenantInFlight: 8, TenantQueue: 8, DBInFlight: 1, DBQueue: 1}})
	r.ex.Set(func(e *fronttest.Executor) { e.Delay = 150 * time.Millisecond })
	tok := r.tok(t, "db-1", nil)
	var wg sync.WaitGroup
	codes := make(chan front.Code, 3)
	for i, timeout := range []time.Duration{time.Second, 30 * time.Millisecond, time.Second} {
		wg.Go(func() {
			codes <- r.s.Handle(context.Background(), front.Input{Ticket: tok, Body: get(1), Timeout: timeout}).Resp.Code
		})
		if i == 0 {
			for r.s.Admission().Stats().InFlight == 0 {
				time.Sleep(time.Millisecond)
			}
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	wg.Wait()
	close(codes)
	got := map[front.Code]int{}
	for c := range codes {
		got[c]++
	}
	if got[front.OK] != 1 || got[front.Expired] != 1 || got[front.Shed] != 1 {
		t.Fatalf("codes %v, want one ok, one expired (queued past its deadline), one shed (queue full)", got)
	}
	r.balanced(t)
}

func TestPipelineTimeoutIsClamped(t *testing.T) {
	r := newRig(t, front.Config{MaxTimeout: 50 * time.Millisecond, DefaultTimeout: time.Hour})
	if d := r.s.Config().DefaultTimeout; d != 50*time.Millisecond {
		t.Fatalf("default %v not clamped to max", d)
	}
	r.ex.Set(func(e *fronttest.Executor) { e.Block = true })
	t0 := time.Now()
	out := r.s.Handle(context.Background(), front.Input{Ticket: r.tok(t, "db-1", nil), Body: get(1), Timeout: time.Hour})
	if out.Resp.Code != front.Expired || time.Since(t0) > time.Second {
		t.Fatalf("%s after %v", out.Resp.Code, time.Since(t0))
	}
}

// Drain: new requests are shed, the in-flight one finishes, the idle channel closes after it.
func TestDrainWaitsForInFlight(t *testing.T) {
	r := newRig(t, front.Config{})
	r.ex.Set(func(e *fronttest.Executor) { e.Delay = 100 * time.Millisecond })
	tok := r.tok(t, "db-1", nil)
	done := make(chan front.Code)
	go func() { done <- r.s.Handle(context.Background(), front.Input{Ticket: tok, Body: get(1)}).Resp.Code }()
	for r.s.InFlight() == 0 {
		time.Sleep(time.Millisecond)
	}
	idle := r.s.Drain()
	if out := r.s.Handle(context.Background(), front.Input{Ticket: tok, Body: get(1)}); out.Resp.Code != front.Shed {
		t.Fatalf("request during drain: %s", out.Resp.Code)
	}
	select {
	case <-idle:
		t.Fatal("idle before the in-flight request finished")
	default:
	}
	if c := <-done; c != front.OK {
		t.Fatalf("in-flight request: %s", c)
	}
	<-idle
	if r.s.Drain() != idle || !r.s.Draining() {
		t.Fatal("second Drain")
	}
}

func BenchmarkPipeline(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "verify"
		if cached {
			name = "cached-ticket"
		}
		b.Run(name, func(b *testing.B) {
			iss := tickettest.NewIssuer("k1")
			v := ticket.Verifier{Keys: ticket.NewKeyRing(iss.Keys()), VM: vm, Skew: time.Second}
			var tv front.TicketVerifier = v
			if cached {
				tv = ticket.NewCache(v, 1024)
			}
			s := front.NewServer(front.Config{}, tv, fronttest.NewAdmitter(map[string]uint64{"db-1": 1}), noopExec{}, nil)
			in := front.Input{Ticket: iss.Mint(b, iss.Claims("db-1", vm, time.Now(), time.Hour)), Body: get(1)}
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if out := s.Handle(context.Background(), in); out.Resp.Code != front.OK {
						b.Fatal(out.Resp.Code)
					}
				}
			})
		})
	}
}

type noopExec struct{}

func (noopExec) Exec(context.Context, front.Call) (front.Reply, error) {
	return front.Reply{Columns: []string{"k", "v"}, Rows: [][]any{{int64(1), "v1"}}, Count: 1}, nil
}
