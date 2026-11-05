package main

import (
	"context"
	"errors"
	"os"

	"github.com/nickstrad/ts-db-go/internal/actor"
	"github.com/nickstrad/ts-db-go/internal/objstore"
	"github.com/nickstrad/ts-db-go/internal/vactor"
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
