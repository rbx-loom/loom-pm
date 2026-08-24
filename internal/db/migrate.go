package db

import (
	"context"
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
// once cannot both apply the same file.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: acquiring a connection to migrate: %w", err)
	}
	defer connection.Release()

	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("db: taking the migration lock: %w", err)
	}
	defer connection.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock)

	_, err = connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("db: creating schema_migrations: %w", err)
	}

	rows, err := connection.Query(ctx, "SELECT name FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("db: reading schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("db: reading schema_migrations: %w", err)
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("db: reading schema_migrations: %w", err)
	}
	rows.Close()

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
		if applied[name] {
			continue
		}

		statements, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("db: reading migration %s: %w", name, err)
		}

		transaction, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("db: starting migration %s: %w", name, err)
		}

		if _, err := transaction.Exec(ctx, string(statements)); err != nil {
			transaction.Rollback(ctx)
			return fmt.Errorf("db: applying migration %s: %w", name, err)
		}

		if _, err := transaction.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name); err != nil {
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
