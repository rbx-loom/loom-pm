// Package auth identifies who is publishing.
//
// A token is 32 random bytes shown to its owner once and stored only as a sha256 hash, so
// a copy of the database does not yield anyone's credentials.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// TokenPrefix marks a Loom token so that secret scanners can recognise one that has been
// committed by accident.
const TokenPrefix = "loom_pat_"

const tokenBytes = 32

var ErrUnauthenticated = errors.New("auth: no such token")

type User struct {
	ID    int64
	Login string
}

// NewToken answers the token to show its owner once, and the hash to store.
func NewToken() (string, []byte, error) {
	secret := make([]byte, tokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", nil, fmt.Errorf("auth: generating a token: %w", err)
	}

	presented := TokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	return presented, HashToken(presented), nil
}

func HashToken(presented string) []byte {
	hash := sha256.Sum256([]byte(presented))
	return hash[:]
}

// BearerToken reads the token out of an Authorization header.
func BearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}

	token = strings.TrimSpace(token)
	return token, token != ""
}

type Store interface {
	// UserByTokenHash answers the owner of a live token, or ErrUnauthenticated. A revoked
	// or expired token is not a live one.
	UserByTokenHash(ctx context.Context, hash []byte) (User, error)
}

type Authenticator struct {
	store Store
}

func New(store Store) *Authenticator {
	return &Authenticator{store: store}
}

// Authenticate resolves an Authorization header to the user it belongs to.
//
// A lookup failure is returned as itself rather than as ErrUnauthenticated: telling a
// publisher their token is bad when the database is down sends them to rotate a
// credential that was never the problem.
func (a *Authenticator) Authenticate(ctx context.Context, header string) (User, error) {
	presented, ok := BearerToken(header)
	if !ok {
		return User{}, fmt.Errorf("the request carries no bearer token: %w", ErrUnauthenticated)
	}

	return a.store.UserByTokenHash(ctx, HashToken(presented))
}
