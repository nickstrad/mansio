package tenancy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/nickstrad/mansio/server/internal/control/locator"
	"github.com/nickstrad/mansio/server/internal/errs"
)

// Principal is who a credential is: the tenant whose database it reaches, and a subject for logs.
type Principal struct {
	Tenant string
	Sub    string // "app:<app>", "key:<id>" or "session"
}

// maxCredential bounds a bearer credential (a new key is 67 bytes, a session token 32).
const maxCredential = 512

// Authenticate returns who token is when used for app (see the package comment for what each mode accepts).
//
//	post: err == nil ⇒ p.Tenant != ""
//	post: err != nil ⇒ errors.Is(err, ErrUnauthenticated) (an *AuthError), errors.Is(err, ErrUnavailable), or ctx's
//	      error; anything else is a bug
func (s *Service) Authenticate(ctx context.Context, token, app string) (Principal, error) {
	switch {
	case token == "":
		return Principal{}, &AuthError{Reason: ReasonMissing}
	case len(token) > maxCredential || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return Principal{}, &AuthError{Reason: ReasonMalformed}
	case s.cfg.Mode == Off:
		return s.appToken(token, app)
	case !s.cfg.AnyApp && !s.apps[app]:
		return Principal{}, &AuthError{Reason: ReasonMismatch} // not an app this agent serves: no lookup, no cache
	}
	now := s.cfg.Clock.Now()
	h := sha256.Sum256([]byte(token))
	s.mu.Lock()
	c, hit := s.cache[h]
	gen := s.gen
	s.mu.Unlock()
	if hit && now.Before(c.expires) {
		return c.p, c.err
	}
	p, looked, err := s.verify(ctx, token, h, now)
	if err != nil && !errors.Is(err, ErrUnauthenticated) {
		return Principal{}, err // the store's failure: never cached
	}
	if looked { // a refusal decided by the credential's shape alone costs nothing to repeat: not cached
		s.remember(h, gen, cached{p: p, err: err, expires: now.Add(s.cfg.CacheTTL)})
	}
	return p, err
}

// Cached reports whether token's answer (an acceptance or a refusal) is cached and fresh: Authenticate then
// reads no store. Callers that rate-limit store reads charge only uncached tokens.
func (s *Service) Cached(token string) bool {
	h := sha256.Sum256([]byte(token))
	now := s.cfg.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, hit := s.cache[h]
	return hit && now.Before(c.expires)
}

// appToken is off mode: token must be app's own token (constant time, also for an unknown app).
func (s *Service) appToken(token, app string) (Principal, error) {
	want, ok := s.cfg.AppTokens[app]
	if !ok {
		want = token + "x" // compare anyway, and fail
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 || !ok {
		return Principal{}, &AuthError{Reason: ReasonMismatch}
	}
	return Principal{Tenant: s.cfg.OffTenant, Sub: "app:" + app}, nil
}

// verify asks the store who token (SHA-256 h) is; looked reports whether it did (else the shape decided).
func (s *Service) verify(ctx context.Context, token string, h [32]byte, now time.Time) (p Principal, looked bool, err error) {
	mismatch := &AuthError{Reason: ReasonMismatch}
	if strings.HasPrefix(token, keyPrefix) {
		var k StoredKey
		var err error
		var want [32]byte
		var sub string
		if id, hash, ok := locator.ParseAPIKey(token); ok {
			k, err = s.store.KeyByID(ctx, id)
			want, sub = hash, "key:"+id
		} else if legacyKey(token) {
			k, err = s.store.KeyByLegacyHash(ctx, h)
			want, sub = h, "key:legacy"
		} else {
			return Principal{}, false, mismatch
		}
		switch {
		case errors.Is(err, ErrNotFound):
			return Principal{}, true, mismatch
		case err != nil:
			return Principal{}, true, storeError(err, "cannot check an api key")
		case subtle.ConstantTimeCompare(k.Hash[:], want[:]) != 1 || k.Revoked || !k.TenantActive:
			return Principal{}, true, mismatch
		}
		return Principal{Tenant: k.Tenant, Sub: sub}, true, nil
	}
	if s.cfg.Mode != Sessions {
		return Principal{}, false, mismatch
	}
	tenant, err := s.store.SessionTenant(ctx, h, now)
	switch {
	case errors.Is(err, ErrNotFound):
		return Principal{}, true, mismatch
	case err != nil:
		return Principal{}, true, storeError(err, "cannot check a session")
	}
	return Principal{Tenant: tenant, Sub: "session"}, true, nil
}

// keyPrefix starts every API key, new or legacy.
const keyPrefix = "mansio_"

var legacyEnc = base64.RawURLEncoding.Strict()

// legacyKey reports whether key has the Deno platform's form: "mansio_" and 32 bytes in base64url.
func legacyKey(key string) bool {
	rest, ok := strings.CutPrefix(key, keyPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	b, err := legacyEnc.DecodeString(rest)
	return err == nil && len(b) == 32
}

// evictBatch is how many entries a full cache drops per insert (amortized: the cache is not scanned).
const evictBatch = 8

// remember caches an answer unless the cache was cleared since gen was read (the answer may predate a revoke).
func (s *Service) remember(h [32]byte, gen uint64, c cached) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	if len(s.cache) >= s.cfg.CacheMax {
		// Random eviction in bounded work: map iteration starts at a random entry; drop a few from there.
		n := 0
		for k := range s.cache {
			if n == evictBatch {
				break
			}
			delete(s.cache, k)
			n++
		}
	}
	s.cache[h] = c
}

// invalidate drops every cached answer: called after every local change, whatever its outcome (a write can
// commit and still fail to answer).
func (s *Service) invalidate() {
	s.mu.Lock()
	clear(s.cache)
	s.gen++
	s.mu.Unlock()
}

// storeError wraps a store failure once: unavailable stays unavailable; ctx errors pass through.
func storeError(err error, what string) error {
	if errors.Is(err, ErrUnavailable) {
		return errs.Wrap(module, err, "%s: the tenancy store is unavailable; nothing changed, retry later", what)
	}
	return errs.Wrap(module, err, "%s", what)
}
