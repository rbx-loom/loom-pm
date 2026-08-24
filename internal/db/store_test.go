package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

// These run against a real Postgres, because what they are checking is the SQL. Point
// LOOM_TEST_DATABASE_URL at a database you do not mind being emptied:
//
//	LOOM_TEST_DATABASE_URL=postgres://localhost/loom_test go test ./internal/db/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("LOOM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set LOOM_TEST_DATABASE_URL to run the database tests")
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatalf("resetting the schema: %v", err)
	}

	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	return pool
}

func seed(t *testing.T, pool *pgxpool.Pool, name string, versions ...string) {
	t.Helper()

	ctx := context.Background()
	parsed, err := pkgname.Parse(name)
	if err != nil {
		t.Fatalf("pkgname.Parse(%q): %v", name, err)
	}

	var userID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO users (github_id, login) VALUES (1, 'publisher')
		 ON CONFLICT (github_id) DO UPDATE SET login = EXCLUDED.login RETURNING id`).Scan(&userID)
	if err != nil {
		t.Fatalf("seeding a user: %v", err)
	}

	var scopeID *int64
	if parsed.IsScoped() {
		var id int64
		err = pool.QueryRow(ctx,
			`INSERT INTO scopes (name, normalized) VALUES ($1, $2)
			 ON CONFLICT (normalized) DO UPDATE SET name = EXCLUDED.name RETURNING id`,
			parsed.Scope(), parsed.NormalizedScope()).Scan(&id)
		if err != nil {
			t.Fatalf("seeding a scope: %v", err)
		}
		scopeID = &id
	}

	var packageID int64
	err = pool.QueryRow(ctx,
		`INSERT INTO packages (scope_id, name, normalized, squat_key) VALUES ($1, $2, $3, $4) RETURNING id`,
		scopeID, parsed.Name(), parsed.Normalized(), parsed.SquatKey()).Scan(&packageID)
	if err != nil {
		t.Fatalf("seeding a package: %v", err)
	}

	for _, text := range versions {
		version, err := semver.ParseVersion(text)
		if err != nil {
			t.Fatalf("semver.ParseVersion(%q): %v", text, err)
		}

		checksum := storage.DigestOf([]byte(text))

		var prerelease, buildMetadata *string
		if version.Prerelease != "" {
			prerelease = &version.Prerelease
		}
		if version.BuildMetadata != "" {
			buildMetadata = &version.BuildMetadata
		}

		var versionID int64
		err = pool.QueryRow(ctx,
			`INSERT INTO versions
			   (package_id, major, minor, patch, prerelease, build_metadata, checksum, size_bytes, realm, published_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'shared', $9) RETURNING id`,
			packageID, version.Major, version.Minor, version.Patch, prerelease, buildMetadata,
			checksum[:], len(text), userID).Scan(&versionID)
		if err != nil {
			t.Fatalf("seeding version %q: %v", text, err)
		}

		_, err = pool.Exec(ctx,
			`INSERT INTO dependencies (version_id, name, requirement, is_dev) VALUES ($1, 'math', '^1.0', false)`,
			versionID)
		if err != nil {
			t.Fatalf("seeding a dependency: %v", err)
		}
	}
}

func mustName(t *testing.T, text string) pkgname.Name {
	t.Helper()

	name, err := pkgname.Parse(text)
	if err != nil {
		t.Fatalf("pkgname.Parse(%q): %v", text, err)
	}

	return name
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testPool(t)

	if err := Migrate(context.Background(), pool); err != nil {
		t.Errorf("migrating a second time: %v", err)
	}
}

func TestPackageReadsEveryVersion(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0", "1.10.0", "2.0.0-alpha")

	pkg, err := NewStore(pool).Package(context.Background(), mustName(t, "serio"))
	if err != nil {
		t.Fatalf("Package: %v", err)
	}

	if len(pkg.Versions) != 4 {
		t.Fatalf("read %d versions, want 4", len(pkg.Versions))
	}

	if pkg.Name.String() != "serio" {
		t.Errorf("name = %q, want serio", pkg.Name)
	}

	for _, version := range pkg.Versions {
		if len(version.Dependencies) != 1 {
			t.Errorf("%s has %d dependencies, want 1", version.Version, len(version.Dependencies))
		}

		if version.Checksum != storage.DigestOf([]byte(version.Version.String())) {
			t.Errorf("%s has the wrong checksum", version.Version)
		}

		if version.Realm != index.RealmShared {
			t.Errorf("%s has realm %q, want shared", version.Version, version.Realm)
		}
	}
}

func TestPackageReadsAScopedPackage(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "scope/serio", "1.0.0")

	pkg, err := NewStore(pool).Package(context.Background(), mustName(t, "scope/serio"))
	if err != nil {
		t.Fatalf("Package: %v", err)
	}

	if pkg.Name.String() != "scope/serio" {
		t.Errorf("name = %q, want scope/serio", pkg.Name)
	}
}

// A scoped and an unscoped package may share a name segment, and looking one up must not
// answer with the other.
func TestPackageDistinguishesScope(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")
	seed(t, pool, "scope/serio", "2.0.0")

	store := NewStore(pool)

	unscoped, err := store.Package(context.Background(), mustName(t, "serio"))
	if err != nil {
		t.Fatalf("Package(serio): %v", err)
	}

	if got := unscoped.Versions[0].Version.String(); got != "1.0.0" {
		t.Errorf("serio resolved to %s, want 1.0.0", got)
	}

	scoped, err := store.Package(context.Background(), mustName(t, "scope/serio"))
	if err != nil {
		t.Fatalf("Package(scope/serio): %v", err)
	}

	if got := scoped.Versions[0].Version.String(); got != "2.0.0" {
		t.Errorf("scope/serio resolved to %s, want 2.0.0", got)
	}
}

func TestPackageIsCaseInsensitive(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "Serio", "1.0.0")

	pkg, err := NewStore(pool).Package(context.Background(), mustName(t, "SERIO"))
	if err != nil {
		t.Fatalf("Package: %v", err)
	}

	if pkg.Name.String() != "Serio" {
		t.Errorf("name = %q, want the casing it was published under", pkg.Name)
	}
}

func TestPackageNotFound(t *testing.T) {
	pool := testPool(t)

	_, err := NewStore(pool).Package(context.Background(), mustName(t, "serio"))
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Package of a missing package = %v, want index.ErrNotFound", err)
	}
}

func TestVersionReadsOne(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0")

	version, err := semver.ParseVersion("1.2.0")
	if err != nil {
		t.Fatalf("semver.ParseVersion: %v", err)
	}

	published, err := NewStore(pool).Version(context.Background(), mustName(t, "serio"), version)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}

	if !published.Version.Equal(version) {
		t.Errorf("read %s, want %s", published.Version, version)
	}
}

// Version identity excludes build metadata, so a lookup for 1.0.0 must find a version
// published as 1.0.0+build.
func TestVersionIgnoresBuildMetadata(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0+build")

	version, err := semver.ParseVersion("1.0.0")
	if err != nil {
		t.Fatalf("semver.ParseVersion: %v", err)
	}

	published, err := NewStore(pool).Version(context.Background(), mustName(t, "serio"), version)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}

	if published.Version.BuildMetadata != "build" {
		t.Errorf("read %s, want the metadata preserved", published.Version)
	}
}

func TestVersionNotFound(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")

	version, err := semver.ParseVersion("9.9.9")
	if err != nil {
		t.Fatalf("semver.ParseVersion: %v", err)
	}

	_, err = NewStore(pool).Version(context.Background(), mustName(t, "serio"), version)
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Version of a missing version = %v, want index.ErrNotFound", err)
	}
}
