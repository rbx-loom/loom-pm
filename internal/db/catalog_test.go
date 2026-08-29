package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rbx-loom/loom-pm/internal/catalog"
	"github.com/rbx-loom/loom-pm/internal/index"
)

// describe gives a seeded package the description a real publish would have left.
func describe(t *testing.T, pool *pgxpool.Pool, name, description string) {
	t.Helper()

	_, err := pool.Exec(context.Background(),
		`UPDATE packages SET description = $2 WHERE normalized = $1`, mustName(t, name).Normalized(), description)
	if err != nil {
		t.Fatalf("describing %s: %v", name, err)
	}
}

// downloaded records fetches against every version of a package, as the usage recorder does.
func downloaded(t *testing.T, pool *pgxpool.Pool, name string, day time.Time, count int64) {
	t.Helper()

	_, err := pool.Exec(context.Background(), `
		INSERT INTO downloads (version_id, day, count)
		SELECT v.id, $2::date, $3
		FROM versions v JOIN packages p ON p.id = v.package_id
		WHERE p.normalized = $1
		ON CONFLICT (version_id, day) DO UPDATE SET count = downloads.count + EXCLUDED.count`,
		mustName(t, name).Normalized(), day, count)
	if err != nil {
		t.Fatalf("recording downloads for %s: %v", name, err)
	}
}

func names(packages []catalog.Summary) []string {
	listed := make([]string, 0, len(packages))
	for _, summary := range packages {
		listed = append(listed, summary.Name.String())
	}

	return listed
}

func TestSearchFindsByName(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")
	seed(t, pool, "math", "1.0.0")

	results, err := NewStore(pool).Search(context.Background(), "serio", 10, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if strings.Join(names(results.Packages), ",") != "serio" {
		t.Errorf("found %v, want serio", names(results.Packages))
	}

	if results.Total != 1 {
		t.Errorf("total = %d, want 1", results.Total)
	}
}

// Everybody types a prefix before they type a word, and a tsvector matches neither.
func TestSearchFindsByPrefix(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")
	seed(t, pool, "math", "1.0.0")

	results, err := NewStore(pool).Search(context.Background(), "ser", 10, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if strings.Join(names(results.Packages), ",") != "serio" {
		t.Errorf("found %v, want serio", names(results.Packages))
	}
}

func TestSearchFindsByDescription(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")
	describe(t, pool, "serio", "Serialisation and encoding for Loom projects")
	seed(t, pool, "math", "1.0.0")
	describe(t, pool, "math", "Vectors and matrices")

	results, err := NewStore(pool).Search(context.Background(), "matrices", 10, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if strings.Join(names(results.Packages), ",") != "math" {
		t.Errorf("found %v, want math", names(results.Packages))
	}
}

// Somebody typing a package's name wants that package before every package that mentions it.
func TestSearchRanksNameAboveDescription(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")
	seed(t, pool, "wrapper", "1.0.0")
	describe(t, pool, "wrapper", "A convenience wrapper around serio")

	results, err := NewStore(pool).Search(context.Background(), "serio", 10, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results.Packages) != 2 {
		t.Fatalf("found %v, want both", names(results.Packages))
	}

	if results.Packages[0].Name.String() != "serio" {
		t.Errorf("found %v, want serio first", names(results.Packages))
	}
}

func TestSearchPages(t *testing.T) {
	pool := testPool(t)
	for _, name := range []string{"alpha", "alphabet", "alphabetical"} {
		seed(t, pool, name, "1.0.0")
	}

	store := NewStore(pool)

	first, err := store.Search(context.Background(), "alpha", 2, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(first.Packages) != 2 || first.Total != 3 {
		t.Fatalf("first page = %v of %d, want 2 of 3", names(first.Packages), first.Total)
	}

	second, err := store.Search(context.Background(), "alpha", 2, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(second.Packages) != 1 || second.Total != 3 {
		t.Errorf("second page = %v of %d, want 1 of 3", names(second.Packages), second.Total)
	}
}

// A search box gets punctuation, quotes and nonsense typed into it, and none of it may
// reach the parser as syntax.
func TestSearchSurvivesHostileQueries(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0")

	store := NewStore(pool)

	for _, query := range []string{"", "   ", "&|!()", "'; DROP TABLE packages; --", "serio & math", "a:*", "<->"} {
		t.Run(query, func(t *testing.T) {
			if _, err := store.Search(context.Background(), query, 10, 0); err != nil {
				t.Errorf("Search(%q): %v", query, err)
			}
		})
	}

	// and the table is still there
	if _, err := store.Search(context.Background(), "serio", 10, 0); err != nil {
		t.Fatalf("Search after the hostile ones: %v", err)
	}
}

func TestSearchCarriesTheLatestVersionAndDownloads(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0", "1.10.0")
	downloaded(t, pool, "serio", time.Now(), 4)

	results, err := NewStore(pool).Search(context.Background(), "serio", 10, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results.Packages) != 1 {
		t.Fatalf("found %v, want serio", names(results.Packages))
	}

	found := results.Packages[0]
	if got := found.Latest.String(); got != "1.10.0" {
		t.Errorf("latest = %s, want 1.10.0 by semver rather than by string", got)
	}

	// three versions, four downloads each
	if found.Downloads != 12 {
		t.Errorf("downloads = %d, want 12", found.Downloads)
	}
}

func TestRecent(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "first", "1.0.0")
	seed(t, pool, "second", "1.0.0")

	_, err := pool.Exec(context.Background(),
		`UPDATE packages SET updated_at = now() - interval '1 day' WHERE normalized = 'first'`)
	if err != nil {
		t.Fatalf("backdating: %v", err)
	}

	listed, err := NewStore(pool).Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}

	if strings.Join(names(listed), ",") != "second,first" {
		t.Errorf("listed %v, want the newest first", names(listed))
	}
}

func TestDetail(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0")
	describe(t, pool, "serio", "Serialisation for Loom")

	ada := user(t, pool, "ada")
	own(t, pool, "serio", ada)

	downloaded(t, pool, "serio", time.Now(), 3)
	downloaded(t, pool, "serio", time.Now().AddDate(0, 0, -60), 5)

	detail, err := NewStore(pool).Detail(context.Background(), mustName(t, "serio"))
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	if detail.Description != "Serialisation for Loom" {
		t.Errorf("description = %q", detail.Description)
	}

	if strings.Join(detail.Owners, ",") != "ada" {
		t.Errorf("owners = %v, want ada", detail.Owners)
	}

	if len(detail.Versions) != 2 {
		t.Fatalf("listed %d versions, want 2", len(detail.Versions))
	}

	// newest first, which is the opposite of the index document's order
	if detail.Versions[0].Version.String() != "1.2.0" {
		t.Errorf("versions[0] = %s, want the newest", detail.Versions[0].Version)
	}

	// two versions, 3 recent and 5 old each
	if detail.Downloads.Total != 16 {
		t.Errorf("total downloads = %d, want 16", detail.Downloads.Total)
	}

	if detail.Downloads.Recent != 6 {
		t.Errorf("recent downloads = %d, want only the last %d days", detail.Downloads.Recent, catalog.RecentDays)
	}

	if detail.Versions[0].SizeBytes == 0 {
		t.Error("a version reported no size")
	}
}

func TestDetailNotFound(t *testing.T) {
	pool := testPool(t)

	_, err := NewStore(pool).Detail(context.Background(), mustName(t, "nothing"))
	if !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Detail = %v, want index.ErrNotFound", err)
	}
}

// Every version being yanked is a package nobody should depend on, and Latest has to say so
// rather than name one anyway.
func TestDetailLatestSkipsYanked(t *testing.T) {
	pool := testPool(t)
	seed(t, pool, "serio", "1.0.0", "1.2.0")

	_, err := pool.Exec(context.Background(),
		`UPDATE versions SET yanked_at = now() WHERE major = 1 AND minor = 2`)
	if err != nil {
		t.Fatalf("yanking: %v", err)
	}

	detail, err := NewStore(pool).Detail(context.Background(), mustName(t, "serio"))
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	latest, ok := detail.Latest()
	if !ok || latest.Version.String() != "1.0.0" {
		t.Errorf("latest = %v (%t), want 1.0.0", latest.Version, ok)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE versions SET yanked_at = now()`); err != nil {
		t.Fatalf("yanking the rest: %v", err)
	}

	detail, err = NewStore(pool).Detail(context.Background(), mustName(t, "serio"))
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	if _, ok := detail.Latest(); ok {
		t.Error("a wholly yanked package named a latest version")
	}
}
