package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeStore struct {
	users map[string]User
	err   error
}

func (f fakeStore) UserByTokenHash(_ context.Context, hash []byte) (User, error) {
	if f.err != nil {
		return User{}, f.err
	}

	user, ok := f.users[string(hash)]
	if !ok {
		return User{}, ErrUnauthenticated
	}

	return user, nil
}

func TestNewTokenIsPrefixedAndOpaque(t *testing.T) {
	presented, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	if !strings.HasPrefix(presented, TokenPrefix) {
		t.Errorf("token %q has no %s prefix, so a secret scanner cannot recognise it", presented, TokenPrefix)
	}

	if len(presented) <= len(TokenPrefix)+32 {
		t.Errorf("token %q carries only %d characters of secret", presented, len(presented)-len(TokenPrefix))
	}

	if !bytes.Equal(hash, HashToken(presented)) {
		t.Error("the returned hash is not the hash of the returned token")
	}

	if strings.Contains(string(hash), presented) {
		t.Error("the hash contains the token")
	}
}

func TestNewTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}

	for range 64 {
		presented, _, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}

		if seen[presented] {
			t.Fatalf("NewToken repeated %q", presented)
		}
		seen[presented] = true
	}
}

func TestHashTokenIsStable(t *testing.T) {
	if !bytes.Equal(HashToken("loom_pat_a"), HashToken("loom_pat_a")) {
		t.Error("HashToken is not stable")
	}

	if bytes.Equal(HashToken("loom_pat_a"), HashToken("loom_pat_b")) {
		t.Error("HashToken collides")
	}
}

func TestBearerToken(t *testing.T) {
	accepted := []struct{ header, want string }{
		{"Bearer loom_pat_x", "loom_pat_x"},
		{"bearer loom_pat_x", "loom_pat_x"},
		{"BEARER loom_pat_x", "loom_pat_x"},
		{"Bearer    loom_pat_x  ", "loom_pat_x"},
	}

	for _, testCase := range accepted {
		t.Run(testCase.header, func(t *testing.T) {
			got, ok := BearerToken(testCase.header)
			if !ok {
				t.Fatalf("BearerToken(%q) refused it", testCase.header)
			}

			if got != testCase.want {
				t.Errorf("BearerToken(%q) = %q, want %q", testCase.header, got, testCase.want)
			}
		})
	}

	refused := []string{"", "Bearer", "Bearer ", "Basic loom_pat_x", "loom_pat_x", "Token loom_pat_x"}
	for _, header := range refused {
		t.Run("refuses "+header, func(t *testing.T) {
			if got, ok := BearerToken(header); ok {
				t.Errorf("BearerToken(%q) = %q, want it refused", header, got)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	presented, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	store := fakeStore{users: map[string]User{string(hash): {ID: 7, Login: "ada"}}}

	user, err := New(store).Authenticate(context.Background(), "Bearer "+presented)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if user.ID != 7 || user.Login != "ada" {
		t.Errorf("authenticated as %+v, want ada", user)
	}
}

func TestAuthenticateRefuses(t *testing.T) {
	_, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	store := fakeStore{users: map[string]User{string(hash): {ID: 7, Login: "ada"}}}
	authenticator := New(store)

	cases := []struct{ name, header string }{
		{"no header", ""},
		{"wrong scheme", "Basic something"},
		{"unknown token", "Bearer loom_pat_never-issued"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := authenticator.Authenticate(context.Background(), testCase.header); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("Authenticate = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// A store failure is not a refusal: answering ErrUnauthenticated would tell a publisher
// their token is bad when the database is merely down.
func TestAuthenticateSeparatesFailureFromRefusal(t *testing.T) {
	broken := fakeStore{err: errors.New("connection refused")}

	_, err := New(broken).Authenticate(context.Background(), "Bearer loom_pat_x")
	if err == nil {
		t.Fatal("Authenticate succeeded against a broken store")
	}

	if errors.Is(err, ErrUnauthenticated) {
		t.Error("a store failure was reported as a bad token")
	}
}
