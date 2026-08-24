package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/manifest"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/publish"
	"github.com/rbx-loom/loom-pm/internal/semver"
)

const uniqueViolation = "23505"

// Record writes a version and its dependencies, creating the package and its first owner
// when the name is free.
//
// All of it is one transaction, because who owns a name and whether a version exists are
// both decided by what else is being written at the same moment. The unique indexes are
// what actually settle a race; the SELECTs only produce a better diagnostic for the
// publisher who lost it.
func (s *Store) Record(ctx context.Context, record publish.Record) error {
	published := record.Payload.Manifest.Package
	if published == nil {
		return errors.New("db: the payload has no [package] to record")
	}

	return pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
		packageID, err := ownedPackage(ctx, transaction, published.Name, record.PublisherID)
		if err != nil {
			return err
		}

		versionID, err := insertVersion(ctx, transaction, packageID, record)
		if err != nil {
			return err
		}

		for _, dependency := range record.Payload.Manifest.Dependencies {
			_, err := transaction.Exec(ctx,
				`INSERT INTO dependencies (version_id, name, requirement, is_dev) VALUES ($1, $2, $3, $4)`,
				versionID, dependency.Name.String(), dependency.Requirement.String(), dependency.Dev)
			if err != nil {
				return fmt.Errorf("db: recording dependency %q: %w", dependency.Name, err)
			}
		}

		_, err = transaction.Exec(ctx, `UPDATE packages SET updated_at = now() WHERE id = $1`, packageID)
		return err
	})
}

// ownedPackage answers the row id of a package the publisher may publish to, creating it
// when the name is free.
func ownedPackage(ctx context.Context, transaction pgx.Tx, name pkgname.Name, publisherID int64) (int64, error) {
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
		name.Normalized(), publisherID,
	).Scan(&packageID, &owned)

	switch {
	case err == nil && owned:
		return packageID, nil
	case err == nil:
		return 0, fmt.Errorf("'%s' is published by someone else: %w", name, publish.ErrNotOwned)
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("db: locating %s: %w", name, err)
	}

	if name.IsScoped() {
		if err := requireScopeMember(ctx, transaction, name, publisherID); err != nil {
			return 0, err
		}
	}

	return createPackage(ctx, transaction, name, publisherID)
}

func requireScopeMember(ctx context.Context, transaction pgx.Tx, name pkgname.Name, publisherID int64) error {
	var member bool

	err := transaction.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM scope_members m
			JOIN scopes s ON s.id = m.scope_id
			WHERE s.normalized = $1 AND m.user_id = $2
		)`,
		name.NormalizedScope(), publisherID,
	).Scan(&member)
	if err != nil {
		return fmt.Errorf("db: checking membership of %q: %w", name.Scope(), err)
	}

	if !member {
		return fmt.Errorf("'%s' is not yours to publish into: %w", name.Scope(), publish.ErrNotScopeMember)
	}

	return nil
}

func createPackage(ctx context.Context, transaction pgx.Tx, name pkgname.Name, publisherID int64) (int64, error) {
	var scopeID *int64
	if name.IsScoped() {
		var id int64
		err := transaction.QueryRow(ctx, `SELECT id FROM scopes WHERE normalized = $1`, name.NormalizedScope()).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("db: locating scope %q: %w", name.Scope(), err)
		}
		scopeID = &id
	}

	var packageID int64
	err := transaction.QueryRow(ctx,
		`INSERT INTO packages (scope_id, name, normalized, squat_key) VALUES ($1, $2, $3, $4) RETURNING id`,
		scopeID, name.Name(), name.Normalized(), name.SquatKey(),
	).Scan(&packageID)

	// squat_key is unique, so a name differing from a taken one only by '-' against '_'
	// collides here rather than being created
	var violation *pgconn.PgError
	if errors.As(err, &violation) && violation.Code == uniqueViolation {
		return 0, fmt.Errorf("'%s' is too close to a package that already exists: %w", name, publish.ErrSquatted)
	}
	if err != nil {
		return 0, fmt.Errorf("db: creating %s: %w", name, err)
	}

	_, err = transaction.Exec(ctx,
		`INSERT INTO package_owners (package_id, user_id) VALUES ($1, $2)`, packageID, publisherID)
	if err != nil {
		return 0, fmt.Errorf("db: recording the first owner of %s: %w", name, err)
	}

	return packageID, nil
}

func insertVersion(ctx context.Context, transaction pgx.Tx, packageID int64, record publish.Record) (int64, error) {
	published := record.Payload.Manifest.Package
	version := published.Version

	var versionID int64
	err := transaction.QueryRow(ctx, `
		INSERT INTO versions
			(package_id, major, minor, patch, prerelease, build_metadata, checksum, size_bytes,
			 edition, license, description, repository, realm, authors, published_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id`,
		packageID, version.Major, version.Minor, version.Patch,
		nilIfEmpty(version.Prerelease), nilIfEmpty(version.BuildMetadata),
		record.Payload.Digest[:], len(record.Payload.Content),
		nilIfEmpty(published.Edition), nilIfEmpty(published.License),
		nilIfEmpty(published.Description), nilIfEmpty(published.Repository),
		string(published.Realm), published.Authors, record.PublisherID,
	).Scan(&versionID)

	// versions_identity is over (package, major, minor, patch, prerelease) and excludes
	// build metadata, so republishing 1.0.0 as 1.0.0+other collides here as it should
	var violation *pgconn.PgError
	if errors.As(err, &violation) && violation.Code == uniqueViolation {
		return 0, fmt.Errorf(
			"'%s' %s is already published; a published version is never replaced, so publish a new one: %w",
			published.Name, version, publish.ErrAlreadyPublished)
	}
	if err != nil {
		return 0, fmt.Errorf("db: recording %s %s: %w", published.Name, version, err)
	}

	return versionID, nil
}

// Unsatisfiable answers the dependencies no published, unyanked version satisfies.
//
// The requirement is measured in Go rather than in SQL because what a requirement accepts
// is the ported semver's answer, and the two must not drift.
func (s *Store) Unsatisfiable(ctx context.Context, dependencies []manifest.Dependency) ([]manifest.Dependency, error) {
	var missing []manifest.Dependency

	for _, dependency := range dependencies {
		id, _, err := s.locate(ctx, dependency.Name)
		if errors.Is(err, index.ErrNotFound) {
			missing = append(missing, dependency)
			continue
		} else if err != nil {
			return nil, err
		}

		versions, err := s.versionsOf(ctx, id, nil)
		if err != nil {
			return nil, err
		}

		if !anySatisfies(versions, dependency.Requirement) {
			missing = append(missing, dependency)
		}
	}

	return missing, nil
}

func anySatisfies(versions []index.Version, requirement semver.Requirement) bool {
	for _, version := range versions {
		if !version.Yanked && requirement.Satisfies(version.Version) {
			return true
		}
	}

	return false
}

// Yank sets or clears a version's yanked mark.
func (s *Store) Yank(ctx context.Context, name pkgname.Name, version semver.Version, yanked bool, userID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(transaction pgx.Tx) error {
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
			name.Normalized(), userID,
		).Scan(&packageID, &owned)

		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s: %w", name, index.ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("db: locating %s: %w", name, err)
		}

		if !owned {
			return fmt.Errorf("'%s' is published by someone else: %w", name, publish.ErrNotOwned)
		}

		mark := "now()"
		if !yanked {
			mark = "NULL"
		}

		tag, err := transaction.Exec(ctx, `
			UPDATE versions SET yanked_at = `+mark+`
			WHERE package_id = $1 AND major = $2 AND minor = $3 AND patch = $4
			  AND COALESCE(prerelease, '') = COALESCE($5::text, '')`,
			packageID, version.Major, version.Minor, version.Patch, nilIfEmpty(version.Prerelease))
		if err != nil {
			return fmt.Errorf("db: yanking %s %s: %w", name, version, err)
		}

		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%s %s: %w", name, version, index.ErrNotFound)
		}

		return nil
	})
}

func nilIfEmpty(text string) *string {
	if text == "" {
		return nil
	}

	return &text
}
