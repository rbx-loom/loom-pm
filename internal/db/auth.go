package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rbx-loom/loom-pm/internal/auth"
)

// UserByTokenHash answers the owner of a live token. Revoked tokens are not live, and the
// lookup is by hash because the token itself is never stored.
func (s *Store) UserByTokenHash(ctx context.Context, hash []byte) (auth.User, error) {
	var user auth.User

	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.login
		FROM tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.hash = $1 AND t.revoked_at IS NULL`,
		hash,
	).Scan(&user.ID, &user.Login)

	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrUnauthenticated
	} else if err != nil {
		return auth.User{}, fmt.Errorf("db: resolving a token: %w", err)
	}

	// best effort: a failed timestamp is not a reason to refuse a valid token
	s.pool.Exec(ctx, `UPDATE tokens SET last_used_at = now() WHERE hash = $1`, hash)

	return user, nil
}

// IssueToken creates a user if their login is new, mints a token for them, and answers the
// token to show once. It exists so a self-hosted registry can mint its first credential
// before anyone can log in through the web.
func (s *Store) IssueToken(ctx context.Context, login, name string) (string, error) {
	presented, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}

	err = pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
		var userID int64

		err := transaction.QueryRow(ctx, `SELECT id FROM users WHERE login = $1`, login).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			// github_id is the identity a real login supplies; a bootstrapped user has
			// none yet, so it is derived and replaced when they first sign in
			err = transaction.QueryRow(ctx,
				`INSERT INTO users (github_id, login) VALUES (-nextval('users_id_seq'), $1) RETURNING id`,
				login).Scan(&userID)
		}
		if err != nil {
			return fmt.Errorf("db: finding or creating %q: %w", login, err)
		}

		_, err = transaction.Exec(ctx,
			`INSERT INTO tokens (user_id, name, hash) VALUES ($1, $2, $3)`, userID, name, hash)
		if err != nil {
			return fmt.Errorf("db: issuing a token for %q: %w", login, err)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return presented, nil
}
