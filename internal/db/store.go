// Package db reads and writes the registry's Postgres schema.
package db

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Package(ctx context.Context, name pkgname.Name) (index.Package, error) {
	id, published, err := s.locate(ctx, name)
	if err != nil {
		return index.Package{}, err
	}

	versions, err := s.versionsOf(ctx, id, nil)
	if err != nil {
		return index.Package{}, err
	}

	return index.Package{Name: published, Versions: versions}, nil
}

func (s *Store) Version(ctx context.Context, name pkgname.Name, version semver.Version) (index.Version, error) {
	id, _, err := s.locate(ctx, name)
	if err != nil {
		return index.Version{}, err
	}

	versions, err := s.versionsOf(ctx, id, &version)
	if err != nil {
		return index.Version{}, err
	}

	if len(versions) == 0 {
		return index.Version{}, fmt.Errorf("%s %s: %w", name, version, index.ErrNotFound)
	}

	return versions[0], nil
}

// Modified answers when a package last changed, without reading its versions. It is the
// query a revalidation costs when the rendered document is already cached.
func (s *Store) Modified(ctx context.Context, name pkgname.Name) (time.Time, error) {
	var modified time.Time

	err := s.pool.QueryRow(ctx,
		`SELECT updated_at FROM packages WHERE normalized = $1`, name.Normalized(),
	).Scan(&modified)

	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%s: %w", name, index.ErrNotFound)
	} else if err != nil {
		return time.Time{}, fmt.Errorf("db: locating %s: %w", name, err)
	}

	return modified, nil
}

// locate answers the row id of a package and the name in the casing it was published
// under, which is what the index document should render.
func (s *Store) locate(ctx context.Context, name pkgname.Name) (int64, pkgname.Name, error) {
	var (
		id    int64
		scope *string
		named string
	)

	err := s.pool.QueryRow(ctx, `
		SELECT p.id, s.name, p.name
		FROM packages p
		LEFT JOIN scopes s ON s.id = p.scope_id
		WHERE p.normalized = $1`,
		name.Normalized(),
	).Scan(&id, &scope, &named)

	if errors.Is(err, pgx.ErrNoRows) {
		return 0, pkgname.Name{}, fmt.Errorf("%s: %w", name, index.ErrNotFound)
	} else if err != nil {
		return 0, pkgname.Name{}, fmt.Errorf("db: locating %s: %w", name, err)
	}

	text := named
	if scope != nil {
		text = *scope + "/" + named
	}

	published, err := pkgname.Parse(text)
	if err != nil {
		return 0, pkgname.Name{}, fmt.Errorf("db: %q is stored but is not a package name: %w", text, err)
	}

	return id, published, nil
}

// versionsOf reads a package's versions with their dependencies, narrowed to one version
// when only is set. Build metadata is excluded from the match because version identity
// excludes it.
func (s *Store) versionsOf(ctx context.Context, packageID int64, only *semver.Version) ([]index.Version, error) {
	var prerelease *string
	if only != nil && only.Prerelease != "" {
		prerelease = &only.Prerelease
	}

	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.major, v.minor, v.patch, v.prerelease, v.build_metadata,
		       v.checksum, v.realm, v.yanked_at IS NOT NULL, v.published_at
		FROM versions v
		WHERE v.package_id = $1
		  AND ($2::boolean IS FALSE OR (
		        v.major = $3 AND v.minor = $4 AND v.patch = $5
		        AND COALESCE(v.prerelease, '') = COALESCE($6::text, '')
		      ))`,
		packageID, only != nil,
		componentOf(only, func(v semver.Version) int { return v.Major }),
		componentOf(only, func(v semver.Version) int { return v.Minor }),
		componentOf(only, func(v semver.Version) int { return v.Patch }),
		prerelease,
	)
	if err != nil {
		return nil, fmt.Errorf("db: reading versions: %w", err)
	}
	defer rows.Close()

	var (
		versions = []index.Version{}
		byID     = map[int64]int{}
	)

	for rows.Next() {
		var (
			id                        int64
			major, minor, patch       int32
			prerelease, buildMetadata *string
			checksum                  []byte
			realm                     string
			version                   index.Version
		)

		err := rows.Scan(&id, &major, &minor, &patch, &prerelease, &buildMetadata,
			&checksum, &realm, &version.Yanked, &version.PublishedAt)
		if err != nil {
			return nil, fmt.Errorf("db: reading a version: %w", err)
		}

		if len(checksum) != len(storage.Digest{}) {
			return nil, fmt.Errorf("db: version %d has a %d byte checksum", id, len(checksum))
		}

		version.Version = semver.Version{
			Major:         int(major),
			Minor:         int(minor),
			Patch:         int(patch),
			Prerelease:    valueOf(prerelease),
			BuildMetadata: valueOf(buildMetadata),
		}
		version.ID = id
		version.Checksum = storage.Digest(checksum)
		version.Realm = index.Realm(realm)
		version.Dependencies = []index.Dependency{}

		byID[id] = len(versions)
		versions = append(versions, version)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: reading versions: %w", err)
	}

	if len(versions) == 0 {
		return versions, nil
	}

	return versions, s.attachDependencies(ctx, byID, versions)
}

// attachDependencies fills in the dependencies of the versions byID indexes.
func (s *Store) attachDependencies(ctx context.Context, byID map[int64]int, versions []index.Version) error {
	rows, err := s.pool.Query(ctx,
		`SELECT version_id, name, requirement, is_dev FROM dependencies WHERE version_id = ANY($1)`,
		slices.Collect(maps.Keys(byID)))
	if err != nil {
		return fmt.Errorf("db: reading dependencies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			versionID       int64
			name, statement string
			dev             bool
		)

		if err := rows.Scan(&versionID, &name, &statement, &dev); err != nil {
			return fmt.Errorf("db: reading a dependency: %w", err)
		}

		parsed, err := pkgname.Parse(name)
		if err != nil {
			return fmt.Errorf("db: dependency %q is stored but is not a package name: %w", name, err)
		}

		requirement, err := semver.ParseRequirement(statement)
		if err != nil {
			return fmt.Errorf("db: %q is stored but is not a version requirement: %w", statement, err)
		}

		at := byID[versionID]
		versions[at].Dependencies = append(versions[at].Dependencies, index.Dependency{
			Name:        parsed,
			Requirement: requirement,
			Dev:         dev,
		})
	}

	return rows.Err()
}

func componentOf(version *semver.Version, read func(semver.Version) int) int {
	if version == nil {
		return 0
	}

	return read(*version)
}

func valueOf(text *string) string {
	if text == nil {
		return ""
	}

	return *text
}
