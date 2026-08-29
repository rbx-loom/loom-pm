package db

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
				`INSERT INTO dependencies (version_id, name, normalized, requirement, is_dev)
				 VALUES ($1, $2, $3, $4, $5)`,
				versionID, dependency.Name.String(), dependency.Name.Normalized(),
				dependency.Requirement.String(), dependency.Dev)
			if err != nil {
				return fmt.Errorf("db: recording dependency %q: %w", dependency.Name, err)
			}
		}

		// description is denormalised onto the package for searching and listing; the most
		// recent publish's wins, so an old patch released afterwards leaves a staler
		// subtitle rather than a wrong one
		_, err = transaction.Exec(ctx,
			`UPDATE packages SET updated_at = now(), description = $2 WHERE id = $1`,
			packageID, nilIfEmpty(published.Description))
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
		record.Payload.Digest[:], record.Payload.Size,
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
// Every candidate version is read in one query rather than three per dependency, because a
// manifest may name MaxDependencies of them and one upload must not become one round trip
// per name. The requirement itself is still measured in Go: what a requirement accepts is
// the ported semver's answer, and the two must not drift.
func (s *Store) Unsatisfiable(ctx context.Context, dependencies []manifest.Dependency) ([]manifest.Dependency, error) {
	keys := make([]string, len(dependencies))
	for index, dependency := range dependencies {
		keys[index] = dependency.Name.Normalized()
	}

	rows, err := s.pool.Query(ctx, `
		SELECT p.normalized, v.major, v.minor, v.patch, v.prerelease
		FROM packages p
		JOIN versions v ON v.package_id = p.id
		WHERE p.normalized = ANY($1) AND v.yanked_at IS NULL`, keys)
	if err != nil {
		return nil, fmt.Errorf("db: reading candidate versions: %w", err)
	}
	defer rows.Close()

	published := map[string][]semver.Version{}
	for rows.Next() {
		var (
			normalized          string
			major, minor, patch int32
			prerelease          *string
		)

		if err := rows.Scan(&normalized, &major, &minor, &patch, &prerelease); err != nil {
			return nil, fmt.Errorf("db: reading a candidate version: %w", err)
		}

		published[normalized] = append(published[normalized], semver.Version{
			Major:      int(major),
			Minor:      int(minor),
			Patch:      int(patch),
			Prerelease: valueOf(prerelease),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: reading candidate versions: %w", err)
	}

	var missing []manifest.Dependency
	for _, dependency := range dependencies {
		if !slices.ContainsFunc(published[dependency.Name.Normalized()], dependency.Requirement.Satisfies) {
			missing = append(missing, dependency)
		}
	}

	return missing, nil
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

		// a yank changes what the index document says, so it changes when the package was
		// last updated, which is what a cached document is held against
		_, err = transaction.Exec(ctx, `UPDATE packages SET updated_at = now() WHERE id = $1`, packageID)
		return err
	})
}

func nilIfEmpty(text string) *string {
	if text == "" {
		return nil
	}

	return &text
}
