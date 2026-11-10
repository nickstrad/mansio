package objstore_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/objstore/s3test"
)

// EnsureBucket is idempotent: a bucket that is ours is success however we learn it, including from the
// store's "already owned by you" when another process of this account (a second agent starting at the same
// moment) created it between our check and our create. A name another account holds is still a failure.
func TestEnsureBucket(t *testing.T) {
	for _, c := range []struct {
		name  string
		state s3test.BucketState
		ok    bool
		puts  int // bucket creates sent
	}{
		{"it exists and is ours: nothing is created", s3test.BucketOwned, true, 0},
		{"it is missing: created", s3test.BucketMissing, true, 1},
		{"ours, created since the check (already owned by you)", s3test.BucketCreatedSinceHead, true, 1},
		{"another account's, created since the check (already exists)", s3test.BucketTakenSinceHead, false, 1},
		{"another account's: the check is refused, nothing is created", s3test.BucketForeign, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := s3test.New()
			t.Cleanup(fake.Close)
			cfg := fakeConfig(fake.Endpoint())
			fake.SetBucket(cfg.Bucket, c.state)
			err := mustStore(t, cfg).EnsureBucket(context.Background())
			if (err == nil) != c.ok {
				t.Fatalf("EnsureBucket = %v, want ok %v", err, c.ok)
			}
			if got := fake.Count(http.MethodPut); got != c.puts {
				t.Fatalf("%d bucket creates sent, want %d", got, c.puts)
			}
		})
	}
}

// Two stores of one account ensure a missing bucket at the same moment, as two agents starting together do:
// both succeed, whichever create lands second.
func TestEnsureBucketFromTwoStartersAtOnce(t *testing.T) {
	for range 20 {
		fake := s3test.New()
		cfg := fakeConfig(fake.Endpoint())
		fake.SetBucket(cfg.Bucket, s3test.BucketMissing)
		stores := []*objstore.Store{mustStore(t, cfg), mustStore(t, cfg)}
		errs := make([]error, len(stores))
		var wg sync.WaitGroup
		for i, st := range stores {
			wg.Go(func() { errs[i] = st.EnsureBucket(context.Background()) })
		}
		wg.Wait()
		fake.Close()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("starter %d: %v", i, err)
			}
		}
	}
}
