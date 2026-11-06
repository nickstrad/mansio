package main

import (
	"context"
	"errors"
	"os"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// s3Fetcher adapts objstore to vactor's Fetcher port.
type s3Fetcher struct{ st *objstore.Store }

func (f s3Fetcher) Fetch(ctx context.Context, key, path string) (vactor.Fetched, error) {
	s, err := f.st.GetToFile(ctx, key, path)
	if errors.Is(err, objstore.ErrNotFound) {
		err = errors.Join(vactor.ErrNotFound, err)
	}
	return vactor.Fetched{Bytes: s.Bytes, TTFB: s.TTFB, Total: s.Total}, err
}

// Size makes s3Fetcher a vactor.Sizer: one HEAD before each fetch lets the host's bulkhead bound plain
// fetches by bytes (MaxFetchBytes), so many large fetches cannot overrun objstore's per-transfer deadline.
func (f s3Fetcher) Size(ctx context.Context, key string) (int64, error) {
	n, err := f.st.Size(ctx, key)
	if errors.Is(err, objstore.ErrNotFound) {
		err = errors.Join(vactor.ErrNotFound, err)
	}
	return n, err
}

// actorOpener adapts internal/actor to vactor's Opener port: one actor (goroutine + connections) per database.
type actorOpener struct{ cfg actor.Config }

func (o actorOpener) Open(path string) (vactor.DB, error) {
	if _, err := os.Stat(path); err != nil { // SQLite would create a missing file; the port forbids that
		return nil, err
	}
	a := actor.New(0, path, o.cfg)
	if err := a.Start(true); err != nil {
		return nil, err
	}
	return actorDB{a}, nil
}

type actorDB struct{ a *actor.Actor }

func (d actorDB) Query(ctx context.Context, q string) error {
	return d.a.Do(ctx, q, actor.IsWrite(q)).Err
}
func (d actorDB) Close() { d.a.Close() }
