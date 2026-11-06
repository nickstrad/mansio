package pg

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/mansio/server/internal/owner"
	"github.com/nickstrad/mansio/server/internal/schemareg"
)

// The schema registry (0006): registration is idempotent per (app, fingerprint) and content-addressed per
// fingerprint; a read is scoped to the app that registered; bad input is refused before anything is sent.
func TestSchemaRegistry(t *testing.T) {
	p, _ := newPlacement(t, nil)
	text := []byte(`{"games":{"fields":{},"storage":"json","indexes":[]}}`)
	fp := schemareg.Fingerprint(text)

	if ok, err := p.AppRegistered(ctx, "tic-tac-toe"); err != nil || ok {
		t.Fatalf("before: %v, %v", ok, err)
	}
	created, err := p.RegisterSchema(ctx, "tic-tac-toe", fp, text)
	if err != nil || !created {
		t.Fatalf("first register: %v, %v", created, err)
	}
	if created, err = p.RegisterSchema(ctx, "tic-tac-toe", fp, text); err != nil || created {
		t.Fatalf("second register (idempotent): %v, %v", created, err)
	}
	// Another app registering the same text shares the stored row and gets its own registration.
	if created, err = p.RegisterSchema(ctx, "chess", fp, text); err != nil || !created {
		t.Fatalf("second app: %v, %v", created, err)
	}
	if ok, err := p.AppRegistered(ctx, "tic-tac-toe"); err != nil || !ok {
		t.Fatalf("after: %v, %v", ok, err)
	}
	got, err := p.GetSchema(ctx, "tic-tac-toe", fp)
	if err != nil || string(got) != string(text) {
		t.Fatalf("GetSchema = %q, %v", got, err)
	}
	other := []byte(`{"x":{"fields":{},"storage":"json","indexes":[]}}`)
	if _, err := p.RegisterSchema(ctx, "chess", schemareg.Fingerprint(other), other); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetSchema(ctx, "tic-tac-toe", schemareg.Fingerprint(other)); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("another app's schema: %v", err)
	}
	if _, err := p.GetSchema(ctx, "tic-tac-toe", schemareg.Fingerprint([]byte("{}"))); !errors.Is(err, owner.ErrNotFound) {
		t.Fatalf("unknown fingerprint: %v", err)
	}

	for _, tc := range []struct {
		name, app, fp string
		text          []byte
	}{
		{"bad app", "Tic", fp, text},
		{"app with a colon", "a:b", fp, text},
		{"wrong fingerprint", "tic-tac-toe", schemareg.Fingerprint(other), text},
		{"indented text", "tic-tac-toe", schemareg.Fingerprint([]byte(" {}")), []byte(" {}")},
		{"oversized text", "tic-tac-toe", "", []byte(`{"a":"` + strings.Repeat("x", schemareg.MaxText) + `"}`)},
	} {
		f := tc.fp
		if f == "" {
			f = schemareg.Fingerprint(tc.text)
		}
		if _, err := p.RegisterSchema(ctx, tc.app, f, tc.text); !errors.Is(err, owner.ErrInvalid) {
			t.Errorf("%s: %v, want owner.ErrInvalid", tc.name, err)
		}
	}
	if _, err := p.GetSchema(ctx, "tic-tac-toe", "sha256:x"); !errors.Is(err, owner.ErrInvalid) {
		t.Errorf("malformed fingerprint: %v", err)
	}
}

// Many workers registering one schema at once (SQL G-22): no error, exactly one sees created per app.
func TestSchemaRegistryConcurrent(t *testing.T) {
	p, _ := newPlacement(t, nil)
	text := []byte(`{"race":{"fields":{},"storage":"json","indexes":[]}}`)
	fp := schemareg.Fingerprint(text)
	const workers = 32
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			created, err := p.RegisterSchema(ctx, []string{"app-a", "app-b"}[i%2], fp, text)
			results <- created
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for c := range results {
		if c {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d registrations reported created, want 2 (one per app)", n)
	}
}
