// Package actordb adapts internal/actor to vactor's Opener port in durable mode: one actor (goroutine +
// connections) per database, with Commits as the dirty check and Snapshot (the SQLite backup API, on a reader
// connection in WAL mode) as the consistent copy.
package actordb

import (
	"context"
	"os"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// Opener opens each database as a started actor with Cfg.
type Opener struct{ Cfg actor.Config }

// Open starts an actor on the existing file at path. It never creates a missing file (the port forbids it).
func (o Opener) Open(path string) (vactor.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	a := actor.New(0, path, o.Cfg)
	if err := a.Start(true); err != nil {
		return nil, err
	}
	return &DB{a}, nil
}

// DB is an open actor; it implements vactor.DurableDB.
type DB struct{ a *actor.Actor }

// Actor is the actor behind the database, for callers that need full results (the front).
func (d *DB) Actor() *actor.Actor { return d.a }

func (d *DB) Query(ctx context.Context, q string) error { return d.a.Do(ctx, q, actor.IsWrite(q)).Err }
func (d *DB) Close()                                    { d.a.Close() }
func (d *DB) Commits() uint64                           { return d.a.Commits() }
func (d *DB) Snapshot(ctx context.Context, path string) (uint64, error) {
	return d.a.Snapshot(ctx, path)
}
