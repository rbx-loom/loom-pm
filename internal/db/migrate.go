package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed all:migrations
var migrations embed.FS

// Migrate applies every migration not yet recorded, in filename order.
//
// Each runs inside a transaction with an advisory lock held, so two instances starting at
// once cannot both apply the same file. Applied files are recorded with a checksum and
// checked against it on every start: an edited migration is a schema that no longer says
// what the code believes it says, and it is silent until something reads a column that was
// never added.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: acquiring a connection to migrate: %w", err)
	}
	defer connection.Release()

	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("db: taking the migration lock: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)
	}()

	_, err = connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			checksum   BYTEA,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		-- nullable, and added separately, because a registry migrated before checksums
		-- existed has rows with nothing to compare against
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum BYTEA`)
	if err != nil {
		return fmt.Errorf("db: creating schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, connection)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("db: reading the migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)

	for _, name := range names {
		statements, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("db: reading migration %s: %w", name, err)
		}

		checksum := sha256.Sum256(statements)

		if recorded, ok := applied[name]; ok {
			// an unchecksummed row predates this check, so it is adopted rather than
			// refused: there is nothing to compare it against
			if recorded == nil {
				_, err := connection.Exec(ctx,
					"UPDATE schema_migrations SET checksum = $2 WHERE name = $1", name, checksum[:])
				if err != nil {
					return fmt.Errorf("db: recording the checksum of %s: %w", name, err)
				}
				continue
			}

			if !bytes.Equal(recorded, checksum[:]) {
				return fmt.Errorf(
					"db: migration %s has changed since it was applied; the database no longer matches it", name)
			}

			continue
		}

		transaction, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("db: starting migration %s: %w", name, err)
		}

		if _, err := transaction.Exec(ctx, string(statements)); err != nil {
			transaction.Rollback(ctx)
			return fmt.Errorf("db: applying migration %s: %w", name, err)
		}

		if _, err := transaction.Exec(ctx,
			"INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)", name, checksum[:]); err != nil {
			transaction.Rollback(ctx)
			return fmt.Errorf("db: recording migration %s: %w", name, err)
		}

		if err := transaction.Commit(ctx); err != nil {
			return fmt.Errorf("db: committing migration %s: %w", name, err)
		}
	}

	return nil
}

const migrationLock = 0x100_0DB

// appliedMigrations answers the checksum recorded for each applied migration. A nil
// checksum is a row written before they were recorded.
func appliedMigrations(ctx context.Context, connection *pgxpool.Conn) (map[string][]byte, error) {
	rows, err := connection.Query(ctx, "SELECT name, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("db: reading schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string][]byte{}
	for rows.Next() {
		var (
			name     string
			checksum []byte
		)

		if err := rows.Scan(&name, &checksum); err != nil {
			return nil, fmt.Errorf("db: reading schema_migrations: %w", err)
		}

		applied[name] = checksum
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: reading schema_migrations: %w", err)
	}

	return applied, nil
}
