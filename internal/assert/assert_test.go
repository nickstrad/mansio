package assert_test

import (
	"strings"
	"testing"

	"github.com/nickstrad/ts-db-go/internal/assert"
)

// Runs in both builds: `go test ./internal/assert` and `go test -tags noassert ./internal/assert`.
func TestThat(t *testing.T) {
	assert.That(true, "never fails") // on point: a true condition never panics

	defer func() {
		p := recover()
		if !assert.Enabled {
			if p != nil {
				t.Fatalf("panicked with assertions disabled: %v", p)
			}
			return
		}
		msg, _ := p.(string)
		if !strings.Contains(msg, "assertion failed: x = 3") {
			t.Fatalf("panic = %v, want an assertion message with the formatted args", p)
		}
	}()
	assert.That(false, "x = %d", 3)
}
