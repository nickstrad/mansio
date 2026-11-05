package panicx

import (
	"strings"
	"testing"
)

func TestFrom(t *testing.T) {
	if From(nil) != nil {
		t.Fatal("From(nil) must be nil")
	}
	var p *Panic
	func() {
		defer func() { p = From(recover()) }()
		panic("boom")
	}()
	if p.Value != "boom" || !strings.Contains(p.Stack, "TestFrom") || p.Error() != "panic: boom" {
		t.Fatalf("%+v", p)
	}
	if From(p) != p {
		t.Fatal("a *Panic must pass through with its stack")
	}
}
