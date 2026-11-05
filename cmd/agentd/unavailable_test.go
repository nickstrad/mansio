package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/front"
	"github.com/nickstrad/ts-db-go/internal/front/fronttest"
	"github.com/nickstrad/ts-db-go/internal/owner"
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
		{"already front.ErrUnavailable", front.ErrUnavailable, front.Unavailable},
		{"owner fenced keeps its class", fmt.Errorf("%w: %w", front.ErrFenced, owner.ErrFenced), front.Fenced},
		{"shed keeps its class", front.ErrShed, front.Shed},
		{"unknown error stays internal", errors.New("disk on fire"), front.Internal},
		{"bare deadline is not unavailable", context.DeadlineExceeded, front.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := asUnavailable(tc.err)
			if c := front.Classify(got); c != tc.want {
				t.Fatalf("Classify(asUnavailable(%v)) = %s, want %s", tc.err, c, tc.want)
			}
			if tc.err != nil && !errors.Is(got, tc.err) {
				t.Fatalf("the cause was lost: %v", got)
			}
		})
	}

	adm := fronttest.NewAdmitter(map[string]uint64{"db-1": 1})
	adm.SetErr(owner.ErrUnavailable)
	if _, err := (depAdmitter{adm}).Admit("db-1", 1); front.Classify(err) != front.Unavailable {
		t.Fatalf("depAdmitter: %v", err)
	}
	ex := fronttest.NewExecutor("db-1")
	ex.Set(func(e *fronttest.Executor) { e.Err = ownerWrapped })
	if _, err := (depExecutor{ex}).Exec(context.Background(), front.Call{DB: "db-1"}); front.Classify(err) != front.Unavailable {
		t.Fatalf("depExecutor: %v", err)
	}
}
