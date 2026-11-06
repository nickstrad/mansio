package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/mansio/server/internal/coldload"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/objstore"
)

// seed builds one template per size class and uploads Count copies of it, skipping objects already
// uploaded at the same size. Upload workers send their errors back; the first one stops the class.
func (a *app) seed(ctx context.Context, st *objstore.Store) error {
	c := a.cfg
	if err := st.EnsureBucket(ctx); err != nil {
		return errs.Wrap(module, err, "cannot seed: bucket unavailable")
	}
	have, err := st.List(ctx, c.Prefix+"/")
	if err != nil {
		return errs.Wrap(module, err, "cannot seed: listing failed")
	}
	for _, s := range c.Sizes {
		t0 := time.Now()
		path, size, err := buildTemplate(c.WorkDir, s)
		if err != nil {
			return errs.Wrap(module, err, "cannot build the %s template in %s", s.Name, c.WorkDir)
		}
		a.progress("seed %s: template %s (%s) in %v", s.Name, path, coldload.Human(size), time.Since(t0).Round(time.Millisecond))

		todo := c.ToUpload(s, size, have)
		if len(todo) == 0 {
			a.progress("seed %s: all %d objects present", s.Name, s.Count)
			continue
		}
		t1 := time.Now()
		if err := a.upload(ctx, st, s, path, todo); err != nil {
			return err
		}
		el := time.Since(t1)
		mbps := float64(size) * float64(len(todo)) / el.Seconds() / 1e6
		a.log.Info("seeded", "class", s.Name, "objects", len(todo), "bytes_each", size, "seconds", el.Seconds(), "mb_per_s", mbps)
		a.progress("seed %s: uploaded %d × %s in %v (%.0f MB/s)", s.Name, len(todo), coldload.Human(size), el.Round(time.Millisecond), mbps)
	}
	return nil
}

// upload sends copies of path for the indexes in todo with UploadWorkers in flight. Workers only report;
// this function decides: stop at the first error (cancelling the rest) but read every outcome.
func (a *app) upload(ctx context.Context, st *objstore.Store, s coldload.SizeClass, path string, todo []int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	errc := make(chan error, len(todo)) // one slot per job: workers never block on reporting
	var done atomic.Int64
	var wg sync.WaitGroup
	for range min(a.cfg.UploadWorkers, len(todo)) {
		wg.Go(func() {
			for i := range jobs {
				_, err := st.PutFile(ctx, a.cfg.Key(s, i), path)
				errc <- err
				if n := done.Add(1); err == nil && n%max(1, int64(len(todo))/10) == 0 {
					a.progress("seed %s: %d/%d uploaded", s.Name, n, len(todo))
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for _, i := range todo {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(errc)
	var first error
	for err := range errc {
		if err != nil && first == nil {
			first = err
			cancel()
		}
	}
	if first != nil {
		return errs.Wrap(module, first, "cannot seed %s: upload failed; re-run seed to resume (uploaded objects are skipped)", s.Name)
	}
	return nil
}
