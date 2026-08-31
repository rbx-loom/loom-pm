package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rbx-loom/loom-pm/internal/auth"
)

// UpsertGitHubUser answers the row a GitHub identity belongs to.
//
// The three cases are ordered, because they overlap: an identity seen before is that user
// whatever they are called now; a login that was bootstrapped from the command line and is
// still waiting for an identity becomes this one, so the packages published with that
// token stay theirs; and anything else is somebody new.
func (s *Store) UpsertGitHubUser(ctx context.Context, identity auth.Identity) (int64, error) {
	var userID int64

	err := pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
		err := transaction.QueryRow(ctx, `
			UPDATE users SET login = $2, avatar_url = $3
			WHERE github_id = $1
			RETURNING id`,
			identity.GitHubID, identity.Login, nilIfEmpty(identity.AvatarURL),
		).Scan(&userID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("db: updating the user for %d: %w", identity.GitHubID, err)
		}

		// a bootstrapped user is one whose github_id is still NULL: they were created from
		// the command line and have never signed in
		err = transaction.QueryRow(ctx, `
			UPDATE users SET github_id = $1, login = $2, avatar_url = $3
			WHERE lower(login) = lower($2) AND github_id IS NULL
			RETURNING id`,
			identity.GitHubID, identity.Login, nilIfEmpty(identity.AvatarURL),
		).Scan(&userID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("db: adopting %q: %w", identity.Login, err)
		}

		err = transaction.QueryRow(ctx,
			`INSERT INTO users (github_id, login, avatar_url) VALUES ($1, $2, $3) RETURNING id`,
			identity.GitHubID, identity.Login, nilIfEmpty(identity.AvatarURL),
		).Scan(&userID)
		if err != nil {
			return fmt.Errorf("db: creating a user for %q: %w", identity.Login, err)
		}

		return nil
	})

	return userID, err
}
