package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/db"
	"github.com/rbx-loom/loom-pm/internal/maintain"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

// sweepGrace is how recently a blob may have been written and still be spared. A publish
// writes its blob before its row, so anything younger than this may belong to one that has
// not committed yet.
const sweepGrace = 24 * time.Hour

// opened is the pool, store and blob store a maintenance command works on. Neither command
// serves anything, so neither starts a server.
func opened(ctx context.Context) (*pgxpool.Pool, *db.Store, *storage.Filesystem, error) {
	settings, err := configure()
	if err != nil {
		return nil, nil, nil, err
	}

	pool, err := pgxpool.New(ctx, settings.databaseURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connecting to the database: %w", err)
	}

	// as the other subcommands do, so a command run before the server has ever started
	// reports on an empty registry rather than on a missing table
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, nil, err
	}

	return pool, db.NewStore(pool), storage.NewFilesystem(settings.blobRoot), nil
}

// sweepBlobs removes blobs no version references.
//
// It reports by default and removes only when asked: the blobs it is deciding about are
// the ones it has just decided are worthless, and being wrong about that is unrecoverable.
func sweepBlobs(arguments []string) error {
	remove := slices.Contains(arguments, "--delete")

	ctx := context.Background()
	pool, store, blobs, err := opened(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	report, err := maintain.Sweep(ctx, blobs, store, sweepGrace, time.Now(), remove)
	if err != nil {
		return err
	}

	fmt.Printf("scanned %d blobs, %d unreferenced\n", report.Scanned, report.Orphaned)

	if report.Spared > 0 {
		fmt.Printf("spared %d written in the last %s, which may belong to a publish in flight\n",
			report.Spared, sweepGrace)
	}

	if !remove {
		removable := report.Orphaned - report.Spared
		fmt.Printf("removed nothing; pass --delete to remove the %d that can go\n", removable)
		return nil
	}

	fmt.Printf("removed %d blobs, reclaiming %s\n", report.Removed, bytes(report.Bytes))
	return nil
}

// verifyBlobs checks that every published version is still installable. It exits non-zero
// when it is not, so a cron or a restore script can act on it.
func verifyBlobs([]string) error {
	ctx := context.Background()
	pool, store, blobs, err := opened(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	report, err := maintain.Verify(ctx, blobs, store)
	if err != nil {
		return err
	}

	for _, published := range report.Missing {
		fmt.Printf("missing: %s (%s)\n", published, published.Checksum)
	}

	for _, published := range report.Corrupt {
		fmt.Printf("corrupt: %s no longer hashes to %s\n", published, published.Checksum)
	}

	fmt.Printf("checked %d published versions: %d missing, %d corrupt\n",
		report.Checked, len(report.Missing), len(report.Corrupt))

	if !report.Sound() {
		return fmt.Errorf("%d published versions cannot be installed", len(report.Missing)+len(report.Corrupt))
	}

	return nil
}

func bytes(count int64) string {
	const unit = 1024

	if count < unit {
		return fmt.Sprintf("%d B", count)
	}

	value, exponent := float64(count)/unit, 0
	for value >= unit && exponent < 3 {
		value /= unit
		exponent++
	}

	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[exponent])
}
