package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rbx-loom/loom-pm/internal/catalog"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
)

// matching is how a search box's contents are turned into rows. Two ways, because one is
// not enough: full text finds words in a description, and a prefix finds the package
// somebody is halfway through typing, which a tsquery never matches.
//
// websearch_to_tsquery takes whatever was typed and never reports a syntax error, which is
// the whole reason to use it rather than to_tsquery.
const matching = `
	FROM packages p
	LEFT JOIN scopes sc ON sc.id = p.scope_id
	WHERE p.search @@ websearch_to_tsquery('english', $1) OR p.normalized LIKE $2 || '%'`

// summaryColumns is what both listings read. Downloads are summed per package rather than
// joined per version, so a package with a hundred versions is still one row out.
const summaryColumns = `
	p.id, p.name, sc.name, p.description, p.updated_at,
	coalesce((
		SELECT sum(d.count) FROM downloads d
		JOIN versions v ON v.id = d.version_id
		WHERE v.package_id = p.id
	), 0)`

func (s *Store) Search(ctx context.Context, query string, limit, offset int) (catalog.Results, error) {
	limit, offset = catalog.Page(limit, offset)
	results := catalog.Results{Query: query, Packages: []catalog.Summary{}}

	terms := searchTerms(query)
	if terms == "" {
		return results, nil
	}

	prefix := strings.ToLower(strings.TrimSpace(query))

	err := s.pool.QueryRow(ctx, `SELECT count(*)`+matching, terms, prefix).Scan(&results.Total)
	if err != nil {
		return catalog.Results{}, fmt.Errorf("db: counting search results: %w", err)
	}

	// an exact name beats a good rank, and a shorter name beats a longer one: somebody
	// typing "serio" wants serio before every package that merely mentions it
	rows, err := s.pool.Query(ctx, `
		SELECT`+summaryColumns+`
		`+matching+`
		ORDER BY (p.normalized = $2) DESC,
			ts_rank(p.search, websearch_to_tsquery('english', $1)) DESC,
			length(p.normalized), p.normalized
		LIMIT $3 OFFSET $4`,
		terms, prefix, limit, offset)
	if err != nil {
		return catalog.Results{}, fmt.Errorf("db: searching: %w", err)
	}
	defer rows.Close()

	results.Packages, err = s.summaries(ctx, rows)
	if err != nil {
		return catalog.Results{}, err
	}

	return results, nil
}

func (s *Store) Recent(ctx context.Context, limit int) ([]catalog.Summary, error) {
	limit, _ = catalog.Page(limit, 0)

	rows, err := s.pool.Query(ctx, `
		SELECT`+summaryColumns+`
		FROM packages p
		LEFT JOIN scopes sc ON sc.id = p.scope_id
		ORDER BY p.updated_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: listing recent packages: %w", err)
	}
	defer rows.Close()

	return s.summaries(ctx, rows)
}

// summaries reads listing rows and fills in each package's latest version.
//
// The versions come back in one further query rather than one per row: a page of twenty
// packages is two round trips, not forty. Which of them is latest is then decided in Go,
// because what "latest" means is semver precedence — prerelease rules included — and that
// is the ported comparison's answer rather than one SQL should be asked to reproduce.
func (s *Store) summaries(ctx context.Context, rows pgx.Rows) ([]catalog.Summary, error) {
	var (
		summaries = []catalog.Summary{}
		ids       []int64
	)

	for rows.Next() {
		var (
			packageID          int64
			name               string
			scope, description *string
			updated            time.Time
			downloads          int64
		)

		if err := rows.Scan(&packageID, &name, &scope, &description, &updated, &downloads); err != nil {
			return nil, fmt.Errorf("db: reading a package summary: %w", err)
		}

		parsed, err := packageNameFrom(scope, name)
		if err != nil {
			return nil, err
		}

		ids = append(ids, packageID)
		summaries = append(summaries, catalog.Summary{
			Name:        parsed,
			Description: valueOf(description),
			Downloads:   downloads,
			UpdatedAt:   updated,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: reading package summaries: %w", err)
	}

	if len(ids) == 0 {
		return summaries, nil
	}

	latest, err := s.latestVersions(ctx, ids)
	if err != nil {
		return nil, err
	}

	for index := range summaries {
		summaries[index].Latest = latest[ids[index]]
	}

	return summaries, nil
}

// latestVersions answers the version to show for each package: the greatest that is not
// yanked, or the greatest of any when every one has been.
func (s *Store) latestVersions(ctx context.Context, ids []int64) (map[int64]semver.Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT package_id, major, minor, patch, prerelease, build_metadata, yanked_at IS NOT NULL
		FROM versions
		WHERE package_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("db: reading versions: %w", err)
	}
	defer rows.Close()

	latest := map[int64]semver.Version{}
	live := map[int64]bool{}

	for rows.Next() {
		var (
			packageID                 int64
			major, minor, patch       int32
			prerelease, buildMetadata *string
			yanked                    bool
		)

		err := rows.Scan(&packageID, &major, &minor, &patch, &prerelease, &buildMetadata, &yanked)
		if err != nil {
			return nil, fmt.Errorf("db: reading a version: %w", err)
		}

		version := semver.Version{
			Major:         int(major),
			Minor:         int(minor),
			Patch:         int(patch),
			Prerelease:    valueOf(prerelease),
			BuildMetadata: valueOf(buildMetadata),
		}

		held, seen := latest[packageID]
		switch {
		case !seen:
			latest[packageID], live[packageID] = version, !yanked
		case live[packageID] && yanked:
			// an unyanked version already stands; a yanked one never displaces it
		case !live[packageID] && !yanked:
			latest[packageID], live[packageID] = version, true
		case version.Compare(held) > 0:
			latest[packageID] = version
		}
	}

	return latest, rows.Err()
}

func (s *Store) Detail(ctx context.Context, name pkgname.Name) (catalog.Detail, error) {
	var (
		packageID          int64
		scope, description *string
		named              string
	)

	err := s.pool.QueryRow(ctx, `
		SELECT p.id, s.name, p.name, p.description
		FROM packages p
		LEFT JOIN scopes s ON s.id = p.scope_id
		WHERE p.normalized = $1`, name.Normalized(),
	).Scan(&packageID, &scope, &named, &description)

	if errors.Is(err, pgx.ErrNoRows) {
		return catalog.Detail{}, fmt.Errorf("%s: %w", name, index.ErrNotFound)
	} else if err != nil {
		return catalog.Detail{}, fmt.Errorf("db: locating %s: %w", name, err)
	}

	parsed, err := packageNameFrom(scope, named)
	if err != nil {
		return catalog.Detail{}, err
	}

	detail := catalog.Detail{
		Name:        parsed,
		Description: valueOf(description),
		Authors:     []string{},
		Owners:      []string{},
		Versions:    []catalog.Version{},
	}

	if err := s.fillVersions(ctx, packageID, &detail); err != nil {
		return catalog.Detail{}, err
	}

	if err := s.fillDownloads(ctx, packageID, &detail); err != nil {
		return catalog.Detail{}, err
	}

	detail.Owners, err = s.Owners(ctx, parsed)
	if err != nil {
		return catalog.Detail{}, err
	}

	return detail, nil
}

// described is a version alongside the metadata that belongs to the package rather than to
// the version, which is carried per version and read from whichever one is newest.
type described struct {
	catalog.Version

	repository string
	license    string
	authors    []string
}

func (s *Store) fillVersions(ctx context.Context, packageID int64, detail *catalog.Detail) error {
	rows, err := s.pool.Query(ctx, `
		SELECT v.major, v.minor, v.patch, v.prerelease, v.build_metadata,
		       v.yanked_at IS NOT NULL, v.published_at, v.size_bytes,
		       v.repository, v.license, v.authors,
		       coalesce((SELECT sum(d.count) FROM downloads d WHERE d.version_id = v.id), 0)
		FROM versions v
		WHERE v.package_id = $1`, packageID)
	if err != nil {
		return fmt.Errorf("db: reading versions: %w", err)
	}
	defer rows.Close()

	var read []described

	for rows.Next() {
		var (
			major, minor, patch       int32
			prerelease, buildMetadata *string
			repository, license       *string
			entry                     described
		)

		err := rows.Scan(&major, &minor, &patch, &prerelease, &buildMetadata,
			&entry.Yanked, &entry.PublishedAt, &entry.SizeBytes,
			&repository, &license, &entry.authors, &entry.Downloads)
		if err != nil {
			return fmt.Errorf("db: reading a version: %w", err)
		}

		entry.Version.Version = semver.Version{
			Major:         int(major),
			Minor:         int(minor),
			Patch:         int(patch),
			Prerelease:    valueOf(prerelease),
			BuildMetadata: valueOf(buildMetadata),
		}
		entry.repository, entry.license = valueOf(repository), valueOf(license)

		read = append(read, entry)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("db: reading versions: %w", err)
	}

	// newest first, which is what a page reads top-down and the opposite of what the index
	// document promises a resolver
	slices.SortFunc(read, func(left, right described) int {
		return right.Version.Version.Compare(left.Version.Version)
	})

	for _, entry := range read {
		detail.Versions = append(detail.Versions, entry.Version)
	}

	// the newest version's metadata is the package's, because that is what somebody
	// depending on it now would get
	if len(read) > 0 {
		detail.Repository, detail.License, detail.Authors = read[0].repository, read[0].license, read[0].authors
	}

	if detail.Authors == nil {
		detail.Authors = []string{}
	}

	return nil
}

func (s *Store) fillDownloads(ctx context.Context, packageID int64, detail *catalog.Detail) error {
	err := s.pool.QueryRow(ctx, `
		SELECT
			coalesce(sum(d.count), 0),
			coalesce(sum(d.count) FILTER (WHERE d.day >= current_date - $2::int), 0)
		FROM downloads d
		JOIN versions v ON v.id = d.version_id
		WHERE v.package_id = $1`, packageID, catalog.RecentDays,
	).Scan(&detail.Downloads.Total, &detail.Downloads.Recent)
	if err != nil {
		return fmt.Errorf("db: counting downloads: %w", err)
	}

	return nil
}

// packageNameFrom rebuilds a package's name from the two columns that hold it.
func packageNameFrom(scope *string, name string) (pkgname.Name, error) {
	text := name
	if scope != nil {
		text = *scope + "/" + name
	}

	parsed, err := pkgname.Parse(text)
	if err != nil {
		return pkgname.Name{}, fmt.Errorf("db: %q is stored but is not a package name: %w", text, err)
	}

	return parsed, nil
}

// searchTerms bounds what reaches the query parser. Length is the only limit worth
// imposing: websearch_to_tsquery takes anything else a search box produces.
func searchTerms(query string) string {
	trimmed := strings.TrimSpace(query)
	if len(trimmed) > 200 {
		trimmed = trimmed[:200]
	}

	return trimmed
}
