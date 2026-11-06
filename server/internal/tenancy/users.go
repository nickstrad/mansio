package tenancy

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/nickstrad/mansio/server/internal/errs"
)

// Sign-up and sign-in (Better Auth's email/password endpoints, as /api/auth serves them). Each answers a
// session token, returned once; the store keeps its SHA-256.

// Token lengths: Better Auth's generateId(32) for both.
const userIDLen, tokenLen = 32, 32

// SignUp creates a user with a password and a first session.
//
//	post: a taken email ⇒ ErrConflict; bad input ⇒ ErrInvalid
func (s *Service) SignUp(ctx context.Context, name, email, password string) (User, string, error) {
	if err := checkText("name", name); err != nil {
		return User{}, "", err
	}
	email, err := checkEmail(email)
	if err != nil {
		return User{}, "", err
	}
	if err := checkPassword(password); err != nil {
		return User{}, "", err
	}
	var hash string
	if err := s.hashing(ctx, func() (err error) { hash, err = hashPassword(s.cfg.Rand, password); return err }); err != nil {
		return User{}, "", err
	}
	id, err := s.alnum(userIDLen)
	if err != nil {
		return User{}, "", err
	}
	token, sess, err := s.newSession(id)
	if err != nil {
		return User{}, "", err
	}
	now := s.cfg.Clock.Now()
	u := User{ID: id, Email: email, Name: name, Created: now, Updated: now}
	if err := s.store.CreateUser(ctx, u, hash, sess); err != nil {
		return User{}, "", storeError(err, "cannot sign up")
	}
	return u, token, nil
}

// SignIn checks email and password and opens a session. An unknown email costs a hash too (no enumeration).
//
//	post: unknown email or wrong password ⇒ an *AuthError (ErrUnauthenticated)
func (s *Service) SignIn(ctx context.Context, email, password string) (User, string, error) {
	email, err := checkEmail(email)
	if err != nil || password == "" || len(password) > 4*maxPassword {
		return User{}, "", &AuthError{Reason: ReasonMismatch}
	}
	u, hash, err := s.store.Password(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		hash = dummyHash
	case err != nil:
		return User{}, "", storeError(err, "cannot sign in")
	}
	var ok bool
	if err := s.hashing(ctx, func() (err error) { ok, err = verifyPassword(hash, password); return err }); err != nil {
		return User{}, "", err
	}
	if !ok || hash == dummyHash {
		return User{}, "", &AuthError{Reason: ReasonMismatch}
	}
	token, sess, err := s.newSession(u.ID)
	if err != nil {
		return User{}, "", err
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return User{}, "", storeError(err, "cannot sign in")
	}
	return u, token, nil
}

// newSession draws a session token for user.
func (s *Service) newSession(user string) (string, Session, error) {
	token, err := s.alnum(tokenLen)
	if err != nil {
		return "", Session{}, err
	}
	return token, Session{Hash: sha256.Sum256([]byte(token)), User: user, Expires: s.cfg.Clock.Now().Add(s.cfg.SessionTTL)}, nil
}

// checkEmail validates and lowercases an email (Better Auth stores them lowercase).
func checkEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if len(e) < 3 || len(e) > 254 || !emailRE.MatchString(e) {
		return "", errs.Wrap(module, ErrInvalid, "email must be an address such as name@example.com")
	}
	return e, nil
}
