package errs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/errs"
)

var errSentinel = errors.New("disk full")

func TestWrap(t *testing.T) {
	low := errs.Wrap("objstore", errSentinel, "cannot download %s", "k").With("bucket", "b")
	top := errs.Wrap("vactor", low, "cannot activate k; nothing cached, safe to retry")

	if got, want := top.Error(), "cannot activate k; nothing cached, safe to retry: cannot download k: disk full"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(top, errSentinel) {
		t.Fatal("errors.Is lost the sentinel through two layers")
	}
	if low.Stack == "" || top.Stack != "" {
		t.Fatalf("stack captured at origin only: low=%v top=%v", low.Stack != "", top.Stack != "")
	}
	if errs.Stack(top) != low.Stack {
		t.Fatal("Stack(top) is not the origin's stack")
	}
	if top.Time.Location().String() != "UTC" {
		t.Fatalf("time not UTC: %v", top.Time.Location())
	}
	chain := errs.Chain(top)
	if len(chain) != 3 || chain[0].Module != "vactor" || chain[1].Misc["bucket"] != "b" || chain[2].Module != "" {
		t.Fatalf("Chain = %+v", chain)
	}
}

func TestOutermost(t *testing.T) {
	e := errs.Wrap("vactor", nil, "x")
	if got, ok := errs.Outermost(e); !ok || got != e {
		t.Fatal("Outermost rejected a well-formed error")
	}
	// A well-formed error wrapped by a raw fmt.Errorf is malformed: some boundary skipped wrapping.
	if _, ok := errs.Outermost(fmt.Errorf("raw: %w", e)); ok {
		t.Fatal("Outermost accepted a raw wrapper")
	}
	if _, ok := errs.Outermost(nil); ok {
		t.Fatal("Outermost accepted nil")
	}
}
