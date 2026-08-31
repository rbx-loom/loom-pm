package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rbx-loom/loom-pm/internal/auth"
)

// CreateSession records a sign-in and answers the value to hand the browser. The secret
// exists outside its hash only in that return value.
//
// Expired rows are removed on the way past. A session table is written once per sign-in
// and read on every page, so sweeping here costs nothing anybody waits on and spares the
// registry a job that exists only to delete rows nothing can reach.
func (s *Store) CreateSession(ctx context.Context, userID int64, lifetime time.Duration) (string, error) {
	presented, hash, err := auth.NewSession()
	if err != nil {
		return "", err
	}

	err = pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
		if _, err := transaction.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
			return fmt.Errorf("db: sweeping expired sessions: %w", err)
		}

		_, err := transaction.Exec(ctx,
			`INSERT INTO sessions (user_id, hash, expires_at) VALUES ($1, $2, now() + $3::interval)`,
			userID, hash, lifetime.String())
		if err != nil {
			return fmt.Errorf("db: recording a session: %w", err)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return presented, nil
}

// UserBySessionHash answers whose sign-in a cookie is, or auth.ErrNoSession.
//
// The expiry is part of the match rather than checked after it, so a session that has run
// out reads exactly like one that was signed out.
func (s *Store) UserBySessionHash(ctx context.Context, hash []byte) (auth.User, error) {
	var user auth.User

	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.login
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.hash = $1 AND s.expires_at > now()`,
		hash,
	).Scan(&user.ID, &user.Login)

	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrNoSession
	} else if err != nil {
		return auth.User{}, fmt.Errorf("db: resolving a session: %w", err)
	}

	return user, nil
}

// RevokeSession ends a sign-in. Deleting rather than marking, because nothing audits the
// closing of a browser tab, and a row kept forever is one more place a hash lives.
//
// Revoking a session that is already gone is not an error: signing out twice has arrived
// at what was asked for.
func (s *Store) RevokeSession(ctx context.Context, hash []byte) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE hash = $1`, hash); err != nil {
		return fmt.Errorf("db: ending a session: %w", err)
	}

	return nil
}
