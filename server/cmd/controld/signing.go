package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/nickstrad/mansio/server/internal/errs"
	"github.com/nickstrad/mansio/server/internal/fsx"
	"github.com/nickstrad/mansio/server/internal/ticket"
)

// keyDir is the issuer's signing key ring on disk (a ticket.KeyStore): one <kid>.json file per key ({kid, seed,
// created}), 0600, in a 0700 directory. rotate runs ticket.Rotate over it.
// One controld per directory (two would each create keys; the agents would still verify both).
type keyDir struct {
	dir    string
	policy ticket.RotationPolicy
	signer *ticket.Signer
}

type keyFile struct {
	KID     string    `json:"kid"`
	Seed    string    `json:"seed"` // base64url Ed25519 seed
	Created time.Time `json:"created"`
}

// rotate brings the ring up to date at now and installs it (ticket.Rotate over this directory). It returns
// what changed, for the log.
func (k *keyDir) rotate(now time.Time) (created string, removed []string, err error) {
	return ticket.Rotate(k, k.signer, k.policy, now)
}

var _ ticket.KeyStore = (*keyDir)(nil)

// Remove deletes a retired key's file.
func (k *keyDir) Remove(kid string) error {
	if err := os.Remove(filepath.Join(k.dir, kid+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errs.Wrap(module, err, "cannot delete retired ticket key %s; it stays published, retried next check", kid)
	}
	return nil
}

// Load reads every key file, refusing a directory or file others can read: signing keys stay
// private to this process's user.
func (k *keyDir) Load() (map[string]ed25519.PrivateKey, []ticket.KeyAge, error) {
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot create ticket_keys.dir").With("path", k.dir)
	}
	fi, err := os.Stat(k.dir)
	if err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot stat ticket_keys.dir").With("path", k.dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, nil, errs.Wrap(module, nil, "ticket_keys.dir %s has mode %#o; make it 0700 (it holds signing keys)", k.dir, fi.Mode().Perm())
	}
	names, err := filepath.Glob(filepath.Join(k.dir, "*.json"))
	if err != nil {
		return nil, nil, errs.Wrap(module, err, "cannot list ticket_keys.dir").With("path", k.dir)
	}
	privs := map[string]ed25519.PrivateKey{}
	var ages []ticket.KeyAge
	for _, name := range names {
		kf, priv, err := readKeyFile(name)
		if err != nil {
			return nil, nil, err
		}
		privs[kf.KID] = priv
		ages = append(ages, ticket.KeyAge{KID: kf.KID, Created: kf.Created})
	}
	return privs, ages, nil
}

func readKeyFile(name string) (keyFile, ed25519.PrivateKey, error) {
	var kf keyFile
	fi, err := os.Stat(name)
	if err != nil {
		return kf, nil, errs.Wrap(module, err, "cannot stat ticket key file").With("path", name)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return kf, nil, errs.Wrap(module, nil, "ticket key file %s has mode %#o; make it 0600", name, fi.Mode().Perm())
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return kf, nil, errs.Wrap(module, err, "cannot read ticket key file").With("path", name)
	}
	if json.Unmarshal(b, &kf) != nil || kf.KID+".json" != filepath.Base(name) || kf.KID == "" || len(kf.KID) > ticket.MaxKID || kf.Created.IsZero() {
		return kf, nil, errs.Wrap(module, nil, "ticket key file %s is not valid: want {kid (= the file name), seed, created}", name)
	}
	priv, err := ticket.ParsePrivateSeed(kf.Seed)
	if err != nil {
		return kf, nil, errs.Wrap(module, nil, "ticket key file %s: bad seed", name)
	}
	return kf, priv, nil
}

// Create writes a new key file atomically (temp file, fsync, rename, fsync the directory).
func (k *keyDir) Create(now time.Time) (ticket.KeyAge, ed25519.PrivateKey, error) {
	var seed [ed25519.SeedSize]byte
	var suffix [3]byte
	rand.Read(seed[:])
	rand.Read(suffix[:])
	kf := keyFile{KID: "k" + now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(suffix[:]), Seed: ticket.EncodeKey(seed[:]),
		Created: now.UTC()}
	b, _ := json.Marshal(kf)
	path := filepath.Join(k.dir, kf.KID+".json")
	fail := func(err error, what string) (ticket.KeyAge, ed25519.PrivateKey, error) {
		return ticket.KeyAge{}, nil, errs.Wrap(module, err, "cannot create a ticket key: %s; the ring is unchanged, retried next check", what).With("path", path)
	}
	if err := fsx.WriteAtomic(path, b, 0o600); err != nil {
		return fail(err, "write")
	}
	return ticket.KeyAge{KID: kf.KID, Created: kf.Created}, ed25519.NewKeyFromSeed(seed[:]), nil
}
