package owner

import (
	"strings"
	"testing"

	"github.com/nickstrad/mansio/server/internal/assert"
)

// Assertions must be able to fire (docs/notes/testing §2).
func TestAssertionsFire(t *testing.T) {
	if !assert.Enabled {
		t.Skip("assertions compiled out (-tags noassert)")
	}
	mustPanic := func(name, want string, f func()) {
		t.Helper()
		defer func() {
			p := recover()
			if msg, _ := p.(string); !strings.Contains(msg, want) {
				t.Errorf("%s: panic = %v, want it to mention %q", name, p, want)
			}
		}()
		f()
	}
	mustPanic("epoch backwards", "epoch went backwards", func() {
		s := newSlot("k", active5, nil)
		s.install(s.view.Load(), Local{State: Active, Epoch: 4})
	})
	mustPanic("double Done", "went negative", func() {
		s := newSlot("k", active5, nil)
		s.inflight.Add(1)
		p := Permit{s}
		p.Done()
		p.Done()
	})
}
