package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nickstrad/mansio/server/internal/actor"
	"github.com/nickstrad/mansio/server/internal/coldload"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/vactor"
)

// run executes every planned level and writes activations.csv, levels.csv and run.json to OutDir.
func (a *app) run(ctx context.Context, st *objstore.Store) (err error) {
	c := a.cfg
	have, err := st.List(ctx, c.Prefix+"/")
	if err != nil {
		return errs.Wrap(module, err, "cannot run: listing failed (is SeaweedFS up?)")
	}
	if missing := c.Missing(have); len(missing) > 0 {
		return errs.Wrap(module, nil, "cannot run: %d objects missing (first %s); run seed first", len(missing), missing[0])
	}

	host, err := vactor.NewHost(s3Fetcher{st}, actorOpener{actor.Config{ReadConns: c.ReadConns, CacheKiB: c.CacheKiB}},
		vactor.Options{CacheDir: c.CacheDir, FirstQuery: c.FirstQuery, WarmQueries: c.WarmQueries,
			KeepLocal: c.KeepLocal, QueryTimeout: c.QueryTimeout.Duration, ActivationTimeout: c.ActivationTimeout.Duration,
			MaxFetches: c.MaxFetches, MaxFetchQueue: c.MaxFetchQueue})
	if err != nil {
		return errs.Wrap(module, err, "cannot run")
	}
	defer func() {
		if cerr := host.Close(); cerr != nil && err == nil { // passivates everything and removes the cache dir
			err = errs.Wrap(module, cerr, "run finished but the cache directory %s was not removed", c.CacheDir)
		}
	}()

	actFile, err := os.Create(filepath.Join(c.OutDir, "activations.csv"))
	if err != nil {
		return errs.Wrap(module, err, "cannot create activations.csv")
	}
	defer actFile.Close()
	acts := newRecorder(actFile, 1<<16)
	defer func() {
		if cerr := acts.Close(); cerr != nil && err == nil {
			err = errs.Wrap(module, cerr, "activations.csv is incomplete")
		}
	}()
	lvlFile, err := os.Create(filepath.Join(c.OutDir, "levels.csv"))
	if err != nil {
		return errs.Wrap(module, err, "cannot create levels.csv")
	}
	defer lvlFile.Close()
	levels := csv.NewWriter(lvlFile)
	levels.Write(coldload.LevelHeader)
	if err := a.writeRunJSON(); err != nil {
		return err
	}

	for _, lv := range c.Levels() {
		if ctx.Err() != nil {
			return errs.Wrap(module, ctx.Err(), "run interrupted before level %d", lv.Index)
		}
		before, fetches0, dropped0 := snapshot(), host.Stats().Fetches, acts.Dropped()
		res := coldload.RunLevel(ctx, host, lv, acts.Add)
		after := snapshot()
		p0 := time.Now()
		host.PassivateAll(64)
		sum := coldload.LevelSummary{Level: lv, Result: res, Fetches: host.Stats().Fetches - fetches0,
			Before: before, After: after, Passivate: time.Since(p0), Dropped: acts.Dropped() - dropped0}
		levels.Write(sum.Row())
		levels.Flush()
		if err := levels.Error(); err != nil {
			return errs.Wrap(module, err, "cannot write levels.csv")
		}
		a.logLevel(sum)
	}
	s := host.Stats()
	a.log.Info("run finished", "activations", s.Activations, "failures", s.Failures, "fetches", s.Fetches, "out_dir", c.OutDir)
	a.progress("done: %d activations, %d failed, %d fetches; output in %s", s.Activations, s.Failures, s.Fetches, c.OutDir)
	return nil
}

func (a *app) logLevel(s coldload.LevelSummary) {
	l, r := s.Level, s.Result
	sec := r.Wall.Seconds()
	a.log.Info("level", "level_index", l.Index, "kind", l.Kind, "class", l.Class.Name, "concurrency", l.Conc,
		"n", len(l.Keys), "ok", r.OK, "failed", r.Failed, "wall_ms", r.Wall.Milliseconds(), "fetches", s.Fetches,
		"bytes", r.Bytes, "rss_mb_after", s.After.RSSMB, "records_dropped", s.Dropped)
	a.progress("level %d %-4s %-6s c=%-4d n=%-5d ok=%d err=%d wall=%v  %.1f act/s  %.0f MB/s  fetches=%d  rss %.0f→%.0f MB",
		l.Index, l.Kind, l.Class.Name, l.Conc, len(l.Keys), r.OK, r.Failed, r.Wall.Round(time.Millisecond),
		float64(r.OK)/sec, float64(r.Bytes)/sec/1e6, s.Fetches, s.Before.RSSMB, s.After.RSSMB)
}

func (a *app) writeRunJSON() error {
	b, _ := json.MarshalIndent(map[string]any{
		"config":  a.cfg,
		"started": time.Now().UTC().Format(time.RFC3339),
		"go":      runtime.Version(),
		"cpus":    runtime.NumCPU(),
		"os":      runtime.GOOS + "/" + runtime.GOARCH,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(a.cfg.OutDir, "run.json"), b, 0o644); err != nil {
		return errs.Wrap(module, err, "cannot write run.json")
	}
	return nil
}
