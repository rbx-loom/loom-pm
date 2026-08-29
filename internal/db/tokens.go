package db

import (
	"context"
	"fmt"
	"time"

	"github.com/rbx-loom/loom-pm/internal/auth"
)

// ListTokens answers a user's live tokens. A revoked token is not listed: it is gone as
// far as its owner is concerned, and the row survives only so an audit can see it did.
func (s *Store) ListTokens(ctx context.Context, userID int64) ([]auth.TokenSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, created_at, last_used_at
		FROM tokens
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: reading tokens: %w", err)
	}
	defer rows.Close()

	summaries := []auth.TokenSummary{}
	for rows.Next() {
		var (
			summary  auth.TokenSummary
			lastUsed *time.Time
		)

		if err := rows.Scan(&summary.ID, &summary.Name, &summary.CreatedAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("db: reading a token: %w", err)
		}

		if lastUsed != nil {
			summary.LastUsedAt = *lastUsed
		}

		summaries = append(summaries, summary)
	}

	return summaries, rows.Err()
}

// CreateToken mints a token for a user who already has one.
//
// This is a token minting another token, which means a leaked one can outlive the
// revocation of itself. It is what the API surface asks for and what npm does; closing it
// means requiring a web session here, which needs a sign-in flow the registry does not
// have yet.
func (s *Store) CreateToken(ctx context.Context, userID int64, name string) (string, auth.TokenSummary, error) {
	presented, hash, err := auth.NewToken()
	if err != nil {
		return "", auth.TokenSummary{}, err
	}

	summary := auth.TokenSummary{Name: name}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO tokens (user_id, name, hash) VALUES ($1, $2, $3) RETURNING id, created_at`,
		userID, name, hash,
	).Scan(&summary.ID, &summary.CreatedAt)
	if err != nil {
		return "", auth.TokenSummary{}, fmt.Errorf("db: issuing a token: %w", err)
	}

	return presented, summary, nil
}

// RevokeToken revokes one of a user's own tokens.
//
// The user id is part of the match rather than checked after it, so revoking somebody
// else's token answers the same as revoking one that does not exist: whose token an id
// belongs to is not something to confirm to whoever does not hold it.
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		tokenID, userID)
	if err != nil {
		return fmt.Errorf("db: revoking a token: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return auth.ErrNoSuchToken
	}

	return nil
}
