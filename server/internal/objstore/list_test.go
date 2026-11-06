package objstore_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/objstore/s3test"
)

// A 10k-object prefix is listed one page (≤ 1000 entries) at a time, and a Transient failure retries only
// the page it hit (RI-05 / OS-06: bounded listings).
func TestEachPageIsBoundedAndRetriesOnePage(t *testing.T) {
	r := newRig(t, objstore.Config{})
	const n = 10_000
	for i := range n {
		r.fake.Put("bkt", fmt.Sprintf("p/%05d", i), []byte("x"))
	}
	r.fake.Put("bkt", "q/outside", []byte("x"))
	var pages, seen int
	last := ""
	err := r.st.EachPage(context.Background(), "p/", "", func(p objstore.Page) error {
		pages++
		if len(p.Objects) > 1000 {
			t.Fatalf("page of %d objects", len(p.Objects))
		}
		for _, o := range p.Objects {
			if o.Key <= last || o.Size != 1 || o.Modified.IsZero() || o.ETag != s3test.ETag([]byte("x")) {
				t.Fatalf("object %+v after %q", o, last)
			}
			last = o.Key
			seen++
		}
		if pages == 3 {
			r.fake.Inject(s3test.Fault{Mode: s3test.Status, Status: 503, Times: 1}) // the 4th page fails once
		}
		return nil
	})
	if err != nil || pages != 10 || seen != n {
		t.Fatalf("EachPage: %d pages, %d objects, %v; want 10, %d", pages, seen, err, n)
	}
	if got := r.fake.Count("GET"); got != 11 {
		t.Fatalf("%d list requests, want 11 (10 pages + 1 retry)", got)
	}
}

// With a delimiter, keys fold into common prefixes, which page and continue like keys.
func TestListPageDelimiter(t *testing.T) {
	r := newRig(t, objstore.Config{})
	for _, k := range []string{"dbs/aa/a/manifest.json", "dbs/aa/a/gen/1", "dbs/aa/b/manifest.json", "dbs/aa/c/gen/1", "dbs/aa/top"} {
		r.fake.Put("bkt", k, []byte("x"))
	}
	var prefixes, objects []string
	token := ""
	for {
		p, err := r.st.ListPage(context.Background(), "dbs/aa/", "/", token, 2)
		if err != nil {
			t.Fatal(err)
		}
		prefixes = append(prefixes, p.Prefixes...)
		for _, o := range p.Objects {
			objects = append(objects, o.Key)
		}
		if token = p.Next; token == "" {
			break
		}
	}
	if !slices.Equal(prefixes, []string{"dbs/aa/a/", "dbs/aa/b/", "dbs/aa/c/"}) || !slices.Equal(objects, []string{"dbs/aa/top"}) {
		t.Fatalf("prefixes %q objects %q", prefixes, objects)
	}
}

// Delete sends batches of at most 1000 keys; missing keys are fine; DeletePrefix empties a prefix of more
// than one page and leaves its neighbours alone.
func TestDeleteBatchesAndDeletePrefix(t *testing.T) {
	r := newRig(t, objstore.Config{})
	var names []string
	for i := range 2500 {
		k := fmt.Sprintf("d/%05d", i)
		r.fake.Put("bkt", k, []byte("x"))
		names = append(names, k)
	}
	r.fake.Put("bkt", "e/keep", []byte("x"))
	if err := r.st.Delete(context.Background(), append(names[:1200:1200], "d/missing")); err != nil {
		t.Fatal(err)
	}
	if got := r.fake.Count("POST"); got != 2 {
		t.Fatalf("%d batch deletes for 1201 keys, want 2", got)
	}
	n, err := r.st.DeletePrefix(context.Background(), "d/")
	if err != nil || n != 1300 {
		t.Fatalf("DeletePrefix = %d, %v; want 1300", n, err)
	}
	if left, _ := r.st.List(context.Background(), ""); len(left) != 1 || left["e/keep"] != 1 {
		t.Fatalf("left %v, want only e/keep", left)
	}
}
