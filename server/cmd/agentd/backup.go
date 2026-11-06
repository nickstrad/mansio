package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nickstrad/mansio/server/internal/durable"
	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/front"
	"github.com/nickstrad/mansio/server/internal/objstore"
	"github.com/nickstrad/mansio/server/internal/owner"
)

// backupAnswer is POST /admin/backup's answer (replace-platform plan phase 4: the Deno platform's
// /admin/backup/:appId over generations): the manifest that now names the key's durable copy (contract 1,
// internal/durable), its version, and whether this call uploaded a generation. Restore a backup with
// `dbtool restore -gen <generation> <key> <path>`.
type backupAnswer struct {
	Code string `json:"code"`
	durable.Manifest
	Version string `json:"version"`
	Wrote   bool   `json:"wrote"` // false: nothing was dirty; the manifest already held every write
}

// backup forces key's write-back and answers the manifest that names its durable copy. Only a key this agent
// holds Active can be backed up: anywhere else its writes are not here to publish.
//
//	post: err == nil ⇒ the answer's generation holds every write acknowledged before the call
func (d *durableAgent) backup(ctx context.Context, key string) (backupAnswer, error) {
	l := d.tab.State(key)
	if l.State != owner.Active {
		return backupAnswer{}, &controlError{"wrong_owner", http.StatusConflict,
			fmt.Sprintf("%s is %s here, not active; back it up on its owner", key, l.State)}
	}
	res, err := d.host.WriteBack(ctx, key)
	if err != nil {
		return backupAnswer{}, classifyBackup(errs.Wrap(module, err, "backup of %s failed", key))
	}
	m, v, err := d.store.ReadManifest(ctx, key)
	if err != nil {
		return backupAnswer{}, classifyBackup(errs.Wrap(module, err, "backup of %s: the write-back finished but the manifest read failed; "+
			"nothing lost, safe to call again", key))
	}
	if m.Epoch != uint64(l.Epoch) {
		// A clean entry's write-back does nothing, so a superseded owner learns it only here: the stored
		// manifest is another epoch's, not this owner's backup.
		return backupAnswer{}, classifyBackup(errs.Wrap(module, durable.ErrStale, "backup of %s: the manifest is at epoch %d, this owner at %d",
			key, m.Epoch, l.Epoch))
	}
	if m.Generation < res.Generation {
		// The manifest never goes back (CheckPublish); an older one means the store broke its contract.
		return backupAnswer{}, errs.Wrap(module, durable.ErrCorrupt, "backup of %s: published generation %d, but the manifest names %d",
			key, res.Generation, m.Generation)
	}
	return backupAnswer{Code: "ok", Manifest: m, Version: string(v), Wrote: res.Wrote}, nil
}

// classifyBackup gives a backup failure the answer classifyControl has no rule for: a higher epoch owns the
// key (stale) or S3 failed transiently (unavailable, not a bug: Release It F2).
func classifyBackup(err error) error {
	switch {
	case errors.Is(err, durable.ErrStale):
		return &controlError{"fenced", http.StatusConflict, "a higher epoch or another writer holds the key; nothing published here"}
	case objstore.Classify(err) == objstore.Transient:
		return fmt.Errorf("%w: %w", durable.ErrUnavailable, err)
	}
	return err
}

// backupHandler is POST /admin/backup?key=K (admin.go): write K back now and answer its manifest.
func (ad *admin) backupHandler(ctx context.Context, r *http.Request) (any, error) {
	key := r.URL.Query().Get("key")
	if err := front.CheckDBID(key); err != nil {
		return nil, badRequest("key: " + err.Error())
	}
	start := time.Now()
	a, err := ad.be.backup(ctx, key)
	ad.events.Emit(backupRecord(key, a, err, time.Since(start)))
	if err != nil {
		return nil, err
	}
	ad.log.Warn("admin: backed up", "event", "ADMIN.BACKUP", "key", key, "generation", a.Generation, "epoch", a.Epoch,
		"wrote", a.Wrote, "remote", r.RemoteAddr)
	return a, nil
}
