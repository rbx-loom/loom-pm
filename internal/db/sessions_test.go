package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rbx-loom/loom-pm/internal/auth"
)

// signedInUser bootstraps somebody to hang a session on.
func signedInUser(t *testing.T, store *Store) int64 {
	t.Helper()

	ctx := context.Background()
	if _, err := store.IssueToken(ctx, "ada", "bootstrap"); err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	var userID int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM users WHERE login = 'ada'`).Scan(&userID); err != nil {
		t.Fatalf("reading the user back: %v", err)
	}

	return userID
}

func TestCreateSessionResolvesToItsUser(t *testing.T) {
	store := NewStore(testPool(t))
	ctx := context.Background()
	userID := signedInUser(t, store)

	presented, err := store.CreateSession(ctx, userID, time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	user, err := store.UserBySessionHash(ctx, auth.HashSession(presented))
	if err != nil {
		t.Fatalf("UserBySessionHash: %v", err)
	}

	if user.ID != userID || user.Login != "ada" {
		t.Errorf("resolved %d/%q, want %d/ada", user.ID, user.Login, userID)
	}
}

// The session is stored the way a token is: only as a hash, so a copy of the database is
// not a set of live sign-ins.
func TestSessionsAreStoredOnlyAsHashes(t *testing.T) {
	store := NewStore(testPool(t))
	ctx := context.Background()

	presented, err := store.CreateSession(ctx, signedInUser(t, store), time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var stored int
	err = store.pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE encode(hash, 'escape') = $1`, presented).Scan(&stored)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}

	if stored != 0 {
		t.Error("the session value itself is in the table")
	}
}

func TestAnExpiredSessionResolvesToNobody(t *testing.T) {
	store := NewStore(testPool(t))
	ctx := context.Background()

	presented, err := store.CreateSession(ctx, signedInUser(t, store), -time.Second)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := store.UserBySessionHash(ctx, auth.HashSession(presented)); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("error = %v, want ErrNoSession", err)
	}
}

func TestRevokeSessionEndsIt(t *testing.T) {
	store := NewStore(testPool(t))
	ctx := context.Background()

	presented, err := store.CreateSession(ctx, signedInUser(t, store), time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	hash := auth.HashSession(presented)
	if err := store.RevokeSession(ctx, hash); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	if _, err := store.UserBySessionHash(ctx, hash); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("error = %v, want ErrNoSession", err)
	}

	// signing out twice has arrived at what was asked for
	if err := store.RevokeSession(ctx, hash); err != nil {
		t.Errorf("revoking an ended session: %v", err)
	}
}

// Nothing runs a job to clear these out, so creating one has to. Otherwise the table only
// ever grows, holding hashes of sign-ins nobody can use.
func TestCreatingASessionSweepsExpiredOnes(t *testing.T) {
	store := NewStore(testPool(t))
	ctx := context.Background()
	userID := signedInUser(t, store)

	if _, err := store.CreateSession(ctx, userID, -time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := store.CreateSession(ctx, userID, time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var rows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}

	if rows != 1 {
		t.Errorf("%d sessions remain, want only the live one", rows)
	}
}

// A user is deleted with their sessions, which is what the cascade is for: a row pointing
// at nobody is a sign-in that would resolve to a join failure rather than to a refusal.
func TestSessionsGoWithTheirUser(t *testing.T) {
	store := NewStore(testPool(t))
	ctx := context.Background()
	userID := signedInUser(t, store)

	if _, err := store.CreateSession(ctx, userID, time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("deleting the user: %v", err)
	}

	var rows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}

	if rows != 0 {
		t.Errorf("%d sessions outlived their user", rows)
	}
}
