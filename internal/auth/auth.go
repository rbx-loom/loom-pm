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
	"time"
)

// TokenPrefix marks a Loom token so that secret scanners can recognise one that has been
// committed by accident.
const TokenPrefix = "loom_pat_"

// SessionPrefix marks the value of a sign-in cookie. Nobody types one, but one does end up
// in a pasted request now and then, and a scanner that knows the shape can say so.
const SessionPrefix = "loom_ses_"

const secretBytes = 32

var ErrUnauthenticated = errors.New("auth: no such token")

// ErrNoSession is answered when a request carries no sign-in, or one that has expired or
// been signed out. It is separate from ErrUnauthenticated because the two are answered
// differently: a request with a good token and no session is refused, not challenged.
var ErrNoSession = errors.New("auth: no such session")

type User struct {
	ID    int64
	Login string

	// TokenHash is the token this user was resolved from, so that its use can be recorded
	// away from the request rather than written on every authenticated call.
	TokenHash []byte
}

// NewToken answers the token to show its owner once, and the hash to store.
func NewToken() (string, []byte, error) { return newSecret(TokenPrefix, "token") }

func HashToken(presented string) []byte { return hashSecret(presented) }

// NewSession answers the value a browser is given and the hash to store. A session is
// generated the same way a token is; what separates them is what each may do.
func NewSession() (string, []byte, error) { return newSecret(SessionPrefix, "session") }

func HashSession(presented string) []byte { return hashSecret(presented) }

func newSecret(prefix, what string) (string, []byte, error) {
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", nil, fmt.Errorf("auth: generating a %s: %w", what, err)
	}

	presented := prefix + base64.RawURLEncoding.EncodeToString(secret)
	return presented, hashSecret(presented), nil
}

func hashSecret(presented string) []byte {
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
	// token is not a live one. Tokens do not expire: nothing in the schema records when
	// one would, and a lifetime nobody enforces is worse than none at all.
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

	hash := HashToken(presented)
	user, err := a.store.UserByTokenHash(ctx, hash)
	if err != nil {
		return User{}, err
	}

	user.TokenHash = hash
	return user, nil
}

var (
	// ErrNoSuchUser is answered when something names a login the registry has never seen.
	ErrNoSuchUser = errors.New("auth: no such user")

	// ErrNoSuchToken is answered when a token is revoked that is not the caller's, which
	// is the same answer as one that does not exist: whose token it is, is not something
	// to confirm to somebody who does not hold it.
	ErrNoSuchToken = errors.New("auth: no such token")
)

// TokenSummary is what a token listing may say about a token: enough to recognise one and
// revoke it, and never the secret, which exists outside its hash exactly once.
type TokenSummary struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}
