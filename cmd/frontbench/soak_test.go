package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/ts-db-go/internal/client"
)

func TestPacerSpacesCallers(t *testing.T) {
	p := newPacer(200) // 5 ms apart
	t0 := time.Now()
	for range 21 {
		p.wait(context.Background())
	}
	if took := time.Since(t0); took < 100*time.Millisecond || took > time.Second {
		t.Fatalf("21 calls at 200/s took %v, want ~100 ms", took)
	}
	newPacer(0).wait(context.Background()) // nil: no wait, no panic
}

func TestPickerSkew(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	pick := picker(r, 100, 1.2)
	n := make([]int, 100)
	for range 100_000 {
		i := pick()
		if i < 0 || i >= 100 {
			t.Fatalf("pick %d out of range", i)
		}
		n[i]++
	}
	if n[0] < 10*n[50] || n[99] == 0 {
		t.Fatalf("not skewed with a live tail: hottest %d, middle %d, coldest %d", n[0], n[50], n[99])
	}
	u := picker(r, 3, 0)
	for range 100 {
		if i := u(); i < 0 || i >= 3 {
			t.Fatalf("uniform pick %d", i)
		}
	}
}

// The acknowledgement log records ok, definite failures and maybes as verify reads them.
func TestSoakLogOutcomes(t *testing.T) {
	dir := t.TempDir()
	acks, evs := filepath.Join(dir, "acks"), filepath.Join(dir, "events")
	l, err := openSoakLog(acks, evs)
	if err != nil {
		t.Fatal(err)
	}
	l.write("fb-0", "a", nil)
	l.write("fb-0", "b", &client.Error{Op: "query", Code: "shed"})
	l.write("fb-1", "c", &client.Error{Op: "query", Code: "internal", MayHaveApplied: true})
	l.write("fb-1", "d", errors.New("not a client error"))
	l.callError("fb-1", "write", time.Millisecond, &client.Error{Op: "query", Code: "unavailable"})
	l.retry(client.RetryEvent{DB: "fb-1", Code: "shed", Hint: time.Second})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(acks)
	if want := "fb-0\ta\tok\nfb-0\tb\tfailed\nfb-1\tc\tunknown\nfb-1\td\tunknown\n"; string(b) != want {
		t.Fatalf("ack log:\n%s\nwant:\n%s", b, want)
	}
	b, _ = os.ReadFile(evs)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[0], `"code":"unavailable"`) || !strings.Contains(lines[1], `"hint_ms":1000`) {
		t.Fatalf("events:\n%s", b)
	}
}
