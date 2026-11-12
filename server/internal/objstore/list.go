package objstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// Listing and deleting: every listing is paged and bounded, and deletes go in batches. LIST is never on a request
// path: GC, cmd/dbtool and benchmark setup only.

// maxPage is S3's largest listing page and largest batch delete.
const maxPage = 1000

// ObjectInfo is one listed object.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time // the store's LastModified
	ETag     string    // without quotes, as GETs report it
}

// Page is one page of a listing.
type Page struct {
	Objects  []ObjectInfo
	Prefixes []string // with a delimiter: the common prefixes ("folders") on this page
	Next     string   // continuation token; "" when this was the last page
}

// ListPage returns one page of at most limit (1 … 1000; out of range means 1000) entries under prefix,
// starting at continuation token token ("" for the first page). With delimiter "/", keys below the next "/"
// fold into Page.Prefixes. One request, retried while Transient, within OpTimeout.
func (s *Store) ListPage(ctx context.Context, prefix, delimiter, token string, limit int) (Page, error) {
	if limit < 1 || limit > maxPage {
		limit = maxPage
	}
	var res minio.ListBucketV2Result
	err := s.retryIdempotent(ctx, OpList, prefix, s.cfg.OpTimeout, func(a *attempt) error {
		// minio's one-page LIST takes no context, so the attempt's deadline and the caller's cancel cannot
		// reach it: it runs on its own goroutine and the attempt stops waiting when its context ends. The
		// abandoned request ends by itself at the transport's response-header timeout (OpTimeout).
		type listed struct {
			res minio.ListBucketV2Result
			err error
		}
		done := make(chan listed, 1)
		go func() {
			r, err := s.core.ListObjectsV2(s.bucket, prefix, "", token, delimiter, limit)
			done <- listed{r, err}
		}()
		select {
		case l := <-done:
			res = l.res
			return l.err
		case <-a.ctx.Done():
			return context.Cause(a.ctx)
		}
	})
	if err != nil {
		return Page{}, wrap(err, "cannot list %s/%s (is the store up?); nothing changed, safe to retry", s.bucket, prefix)
	}
	p := Page{Objects: make([]ObjectInfo, 0, len(res.Contents))}
	for _, o := range res.Contents {
		p.Objects = append(p.Objects, ObjectInfo{Key: o.Key, Size: o.Size, Modified: o.LastModified, ETag: strings.Trim(o.ETag, `"`)})
	}
	for _, c := range res.CommonPrefixes {
		p.Prefixes = append(p.Prefixes, c.Prefix)
	}
	if res.IsTruncated {
		p.Next = res.NextContinuationToken
	}
	return p, nil
}

// EachPage calls fn with every page under prefix, in key order, one page in memory at a time. An error from
// fn stops the listing and is returned as is.
func (s *Store) EachPage(ctx context.Context, prefix, delimiter string, fn func(Page) error) error {
	token := ""
	for {
		p, err := s.ListPage(ctx, prefix, delimiter, token, maxPage)
		if err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
		if p.Next == "" {
			return nil
		}
		token = p.Next
	}
}

// List returns key → size for every object under prefix (benchmark setup: the caller expects a bounded
// prefix; use EachPage for unbounded ones).
func (s *Store) List(ctx context.Context, prefix string) (map[string]int64, error) {
	out := make(map[string]int64)
	err := s.EachPage(ctx, prefix, "", func(p Page) error {
		for _, o := range p.Objects {
			out[o.Key] = o.Size
		}
		return nil
	})
	return out, err
}

// Delete removes the named objects with batch deletes of up to 1000 keys, each retried while Transient
// (DELETE is idempotent; a missing key is not an error).
//
//	post: err == nil ⇒ none of names exists
func (s *Store) Delete(ctx context.Context, names []string) error {
	for len(names) > 0 {
		batch := names[:min(len(names), maxPage)]
		names = names[len(batch):]
		err := s.retryIdempotent(ctx, OpDelete, batch[0], s.cfg.OpTimeout, func(a *attempt) error {
			objs := make(chan minio.ObjectInfo, len(batch)) // buffered to the count: the feeder never blocks
			for _, k := range batch {
				objs <- minio.ObjectInfo{Key: k}
			}
			close(objs)
			var first error
			for e := range s.c.RemoveObjects(a.ctx, s.bucket, objs, minio.RemoveObjectsOptions{}) { // reports failures only
				if e.Err != nil && first == nil {
					first = fmt.Errorf("delete %s: %w", e.ObjectName, e.Err)
				}
			}
			return first
		})
		if err != nil {
			return wrap(err, "cannot delete %d objects from %s (first %s); some may remain, safe to retry", len(batch), s.bucket, batch[0])
		}
	}
	return nil
}

// DeletePrefix removes every object under prefix, one listing page at a time, and returns how many it
// removed.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	n, prev := 0, ""
	for {
		p, err := s.ListPage(ctx, prefix, "", "", maxPage) // always the first page: what we deleted is gone
		if err != nil || len(p.Objects) == 0 {
			return n, err
		}
		if p.Objects[0].Key == prev { // listed again after a delete that reported success
			return n, errs.Wrap(module, nil, "%s under %s was not deleted; safe to retry", p.Objects[0].Key, prefix)
		}
		keys := make([]string, len(p.Objects))
		for i, o := range p.Objects {
			keys[i] = o.Key
		}
		if err := s.Delete(ctx, keys); err != nil {
			return n, err
		}
		n, prev = n+len(keys), keys[0]
	}
}
