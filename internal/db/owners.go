package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rbx-loom/loom-pm/internal/auth"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
)

// Owners answers the logins that may publish a package, ordered so a listing does not
// churn between requests.
func (s *Store) Owners(ctx context.Context, name pkgname.Name) ([]string, error) {
	packageID, _, err := s.locate(ctx, name)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT u.login
		FROM package_owners o
		JOIN users u ON u.id = o.user_id
		WHERE o.package_id = $1
		ORDER BY lower(u.login)`, packageID)
	if err != nil {
		return nil, fmt.Errorf("db: reading the owners of %s: %w", name, err)
	}
	defer rows.Close()

	owners := []string{}
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, fmt.Errorf("db: reading an owner of %s: %w", name, err)
		}

		owners = append(owners, login)
	}

	return owners, rows.Err()
}

// AddOwner gives login the right to publish name. It is idempotent: an owner added twice
// is an owner.
func (s *Store) AddOwner(ctx context.Context, name pkgname.Name, login string, actorID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
		packageID, targetID, err := ownersOf(ctx, transaction, name, login, actorID)
		if err != nil {
			return err
		}

		_, err = transaction.Exec(ctx,
			`INSERT INTO package_owners (package_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			packageID, targetID)
		if err != nil {
			return fmt.Errorf("db: making %q an owner of %s: %w", login, name, err)
		}

		return nil
	})
}

// RemoveOwner takes away login's right to publish name, refusing to remove the last one.
//
// Removing somebody who is not an owner succeeds and changes nothing: the caller asked for
// a state, and that state already holds.
func (s *Store) RemoveOwner(ctx context.Context, name pkgname.Name, login string, actorID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
		packageID, targetID, err := ownersOf(ctx, transaction, name, login, actorID)
		if err != nil {
			return err
		}

		var (
			owners  int
			isOwner bool
		)

		err = transaction.QueryRow(ctx,
			`SELECT count(*), coalesce(bool_or(user_id = $2), false) FROM package_owners WHERE package_id = $1`,
			packageID, targetID,
		).Scan(&owners, &isOwner)
		if err != nil {
			return fmt.Errorf("db: counting the owners of %s: %w", name, err)
		}

		if !isOwner {
			return nil
		}

		if owners == 1 {
			return fmt.Errorf("'%s' would be left with no owner: %w", name, publish.ErrLastOwner)
		}

		_, err = transaction.Exec(ctx,
			`DELETE FROM package_owners WHERE package_id = $1 AND user_id = $2`, packageID, targetID)
		if err != nil {
			return fmt.Errorf("db: removing %q as an owner of %s: %w", login, name, err)
		}

		return nil
	})
}

// ownersOf resolves the package and the login a change names, and refuses an actor who is
// not already an owner. Who may change owners is the same question as who may publish.
func ownersOf(ctx context.Context, transaction pgx.Tx, name pkgname.Name, login string, actorID int64) (int64, int64, error) {
	var (
		packageID int64
		owned     bool
	)

	err := transaction.QueryRow(ctx, `
		SELECT p.id, EXISTS (
			SELECT 1 FROM package_owners o WHERE o.package_id = p.id AND o.user_id = $2
		)
		FROM packages p
		WHERE p.normalized = $1`,
		name.Normalized(), actorID,
	).Scan(&packageID, &owned)

	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("%s: %w", name, index.ErrNotFound)
	} else if err != nil {
		return 0, 0, fmt.Errorf("db: locating %s: %w", name, err)
	}

	if !owned {
		return 0, 0, fmt.Errorf("'%s' is published by someone else: %w", name, publish.ErrNotOwned)
	}

	var targetID int64
	err = transaction.QueryRow(ctx, `SELECT id FROM users WHERE lower(login) = lower($1)`, login).Scan(&targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("%q has never been seen here: %w", login, auth.ErrNoSuchUser)
	} else if err != nil {
		return 0, 0, fmt.Errorf("db: locating %q: %w", login, err)
	}

	return packageID, targetID, nil
}
