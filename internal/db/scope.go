package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
)

// ErrNoSuchUser is answered when a scope names an owner who has never been seen. It is
// auth's sentinel so that the serving path can recognise it without importing this package.
var ErrNoSuchUser = auth.ErrNoSuchUser

// CreateScope registers a scope and makes login its owner.
//
// Publishing into a scope requires a membership row, and nothing on the serving path
// creates one: without this, a fresh registry refuses every scoped name it is asked for.
// It is the same bootstrap IssueToken is, and it requires the user to exist for the same
// reason a scope has to belong to somebody.
func (s *Store) CreateScope(ctx context.Context, scope, login string) error {
	if err := pkgname.ValidateScope(scope); err != nil {
		return err
	}

	scope = strings.TrimSpace(scope)

	return pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
		var userID int64
		err := transaction.QueryRow(ctx, `SELECT id FROM users WHERE login = $1`, login).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%q has never been seen here; run 'loomreg token %s' first: %w",
				login, login, ErrNoSuchUser)
		}
		if err != nil {
			return fmt.Errorf("db: locating %q: %w", login, err)
		}

		var scopeID int64
		err = transaction.QueryRow(ctx,
			`INSERT INTO scopes (name, normalized) VALUES ($1, $2) RETURNING id`,
			scope, strings.ToLower(scope),
		).Scan(&scopeID)

		var violation *pgconn.PgError
		if errors.As(err, &violation) && violation.Code == uniqueViolation {
			return fmt.Errorf("the scope %q already exists", scope)
		}
		if err != nil {
			return fmt.Errorf("db: creating the scope %q: %w", scope, err)
		}

		_, err = transaction.Exec(ctx,
			`INSERT INTO scope_members (scope_id, user_id, role) VALUES ($1, $2, 'owner')`,
			scopeID, userID)
		if err != nil {
			return fmt.Errorf("db: making %q the owner of %q: %w", login, scope, err)
		}

		return nil
	})
}
