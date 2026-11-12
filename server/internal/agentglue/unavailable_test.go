package agentglue

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nickstrad/mansio/server/internal/breaker"
	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/durable/durabletest"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/front/fronttest"
	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/placement/pg"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// Every dependency source agentd wires today, through the adapters, to its wire code: unavailable sources are
// 503 unavailable (never internal); other errors keep their class.
func TestDependencyErrorsMapToUnavailable(t *testing.T) {
	ownerWrapped := errs.Wrap("owner", fmt.Errorf("%w: %w", owner.ErrUnavailable, errors.New("pg: connection refused")),
		"cannot claim \"db-1\": authority unreadable; nothing changed, safe to retry")
	tests := []struct {
		name string
		err  error
		want front.Code
	}{
		{"nil stays ok", nil, front.OK},
		{"owner not reconciled (bare sentinel, as Admit returns it)", owner.ErrUnavailable, front.Unavailable},
		{"owner authority unreadable (wrapped)", ownerWrapped, front.Unavailable},
		{"fetch bulkhead full", errs.Wrap("vactor", vactor.ErrBusy, "cannot activate db-1"), front.Unavailable},
		{"activation deadline", errs.Wrap("vactor", fmt.Errorf("%w: %w", vactor.ErrBusy, context.DeadlineExceeded), "cannot activate db-1"), front.Unavailable},
		{"placement authority down (pg adapter)", errs.Wrap("placement", pg.ErrUnavailable, "cannot read the placement of \"db-1\""), front.Unavailable},
		{"already front.ErrUnavailable", front.ErrUnavailable, front.Unavailable},
		{"owner fenced keeps its class", fmt.Errorf("%w: %w", front.ErrFenced, owner.ErrFenced), front.Fenced},
		{"shed keeps its class", front.ErrShed, front.Shed},
		{"unknown error stays internal", errors.New("disk on fire"), front.Internal},
		{"bare deadline is not unavailable", context.DeadlineExceeded, front.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AsUnavailable(tc.err)
			if c := front.Classify(got); c != tc.want {
				t.Fatalf("Classify(AsUnavailable(%v)) = %s, want %s", tc.err, c, tc.want)
			}
			if tc.err != nil && !errors.Is(got, tc.err) {
				t.Fatalf("the cause was lost: %v", got)
			}
		})
	}

	adm := fronttest.NewAdmitter(map[string]uint64{"db-1": 1})
	adm.SetErr(owner.ErrUnavailable)
	if _, err := (DepAdmitter{adm}).Admit("db-1", 1); front.Classify(err) != front.Unavailable {
		t.Fatalf("depAdmitter: %v", err)
	}
	ex := fronttest.NewExecutor("db-1")
	ex.Set(func(e *fronttest.Executor) { e.Err = ownerWrapped })
	if _, err := (DepExecutor{ex}).Exec(context.Background(), front.Call{DB: "db-1"}); front.Classify(err) != front.Unavailable {
		t.Fatalf("depExecutor: %v", err)
	}
}

// An open S3 breaker (durable.WithBreaker) reaches the wire as 503 unavailable, and its time to half-open
// rides along as the retry hint; a plain S3 system failure is unavailable too.
func TestStorageUnavailableCarriesRetryHint(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	b, err := breaker.New(breaker.Config{MinCalls: 1, FailureRate: 1, CoolDown: 7 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fake := durabletest.NewStore()
	fake.SetFaults(durabletest.Faults{Down: true})
	s := durable.WithBreaker(fake, b, nil, func() time.Time { return now })
	_, _, failed := s.ReadManifest(context.Background(), "db-1")
	_, _, open := s.ReadManifest(context.Background(), "db-1")
	for name, err := range map[string]error{"system failure": failed, "breaker open": open} {
		got := AsUnavailable(errs.Wrap("vactor", err, "cannot activate db-1"))
		if c := front.Classify(got); c != front.Unavailable {
			t.Fatalf("%s: Classify = %s, want unavailable", name, c)
		}
	}
	var hint front.RetryHint
	if !errors.As(AsUnavailable(open), &hint) || hint.RetryAfter() != 7*time.Second {
		t.Fatalf("no 7 s retry hint in %v", open)
	}
}
