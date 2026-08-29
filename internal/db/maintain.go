package db

import (
	"context"
	"fmt"

	"github.com/rbx-loom/loom-pm/internal/maintain"
	"github.com/rbx-loom/loom-pm/internal/semver"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

// Checksums answers every checksum a version references.
//
// Yanked versions are included: a lock file pinning one still installs it, so its blob is
// still referenced and sweeping it would break exactly the builds yanking is careful not to.
func (s *Store) Checksums(ctx context.Context) (map[storage.Digest]struct{}, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT checksum FROM versions`)
	if err != nil {
		return nil, fmt.Errorf("db: reading checksums: %w", err)
	}
	defer rows.Close()

	live := map[storage.Digest]struct{}{}
	for rows.Next() {
		var checksum []byte
		if err := rows.Scan(&checksum); err != nil {
			return nil, fmt.Errorf("db: reading a checksum: %w", err)
		}

		if len(checksum) != len(storage.Digest{}) {
			return nil, fmt.Errorf("db: a version has a %d byte checksum", len(checksum))
		}

		live[storage.Digest(checksum)] = struct{}{}
	}

	return live, rows.Err()
}

// EachPublication visits every published version. An error from fn stops the walk and is
// returned as-is.
func (s *Store) EachPublication(ctx context.Context, fn func(maintain.Publication) error) error {
	rows, err := s.pool.Query(ctx, `
		SELECT s.name, p.name, v.major, v.minor, v.patch, v.prerelease, v.build_metadata, v.checksum
		FROM versions v
		JOIN packages p ON p.id = v.package_id
		LEFT JOIN scopes s ON s.id = p.scope_id
		ORDER BY p.normalized, v.major, v.minor, v.patch`)
	if err != nil {
		return fmt.Errorf("db: reading publications: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			scope, prerelease, buildMetadata *string
			name                             string
			major, minor, patch              int32
			checksum                         []byte
		)

		err := rows.Scan(&scope, &name, &major, &minor, &patch, &prerelease, &buildMetadata, &checksum)
		if err != nil {
			return fmt.Errorf("db: reading a publication: %w", err)
		}

		if len(checksum) != len(storage.Digest{}) {
			return fmt.Errorf("db: %q has a %d byte checksum", name, len(checksum))
		}

		if scope != nil {
			name = *scope + "/" + name
		}

		version := semver.Version{
			Major:         int(major),
			Minor:         int(minor),
			Patch:         int(patch),
			Prerelease:    valueOf(prerelease),
			BuildMetadata: valueOf(buildMetadata),
		}

		err = fn(maintain.Publication{
			Name:     name,
			Version:  version.String(),
			Checksum: storage.Digest(checksum),
		})
		if err != nil {
			return err
		}
	}

	return rows.Err()
}
