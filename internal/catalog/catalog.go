// Package catalog is the registry as a person browses it, rather than as a resolver reads
// it.
//
// The index document answers what a build needs and nothing else: versions, dependencies,
// checksums. Everything here — descriptions, owners, download counts — exists because
// somebody is deciding whether to depend on a package, which is a different question.
package catalog

import (
	"context"
	"time"

	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
)

// Summary is a package as a listing shows it.
type Summary struct {
	Name        pkgname.Name
	Description string
	Latest      semver.Version
	Downloads   int64
	UpdatedAt   time.Time
}

// Downloads counts what a package has been fetched, in total and lately. Recent is what
// tells a reader whether anybody is using it now, which a total cannot.
type Downloads struct {
	Total  int64
	Recent int64
}

// RecentDays is the window Downloads.Recent covers.
const RecentDays = 30

type Detail struct {
	Name        pkgname.Name
	Description string
	Repository  string
	License     string
	Authors     []string
	Owners      []string
	Downloads   Downloads
	Versions    []Version
}

// Version is one published version as a package page lists it, newest first.
type Version struct {
	Version     semver.Version
	Yanked      bool
	PublishedAt time.Time
	Downloads   int64
	SizeBytes   int64
}

// Latest is the newest version anybody should depend on: the greatest that is not yanked,
// or nothing when every version has been.
func (d Detail) Latest() (Version, bool) {
	for _, version := range d.Versions {
		if !version.Yanked {
			return version, true
		}
	}

	return Version{}, false
}

// Results is one page of a search.
type Results struct {
	Query    string
	Total    int
	Packages []Summary
}

// MaxLimit bounds a page, so a listing cannot be asked to read the whole registry.
const MaxLimit = 100

// DefaultLimit is the page size when a caller names none.
const DefaultLimit = 20

// Page is a limit and offset a caller asked for, brought inside what the registry will do.
func Page(limit, offset int) (int, int) {
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit = MaxLimit
	}

	if offset < 0 {
		offset = 0
	}

	return limit, offset
}

type Store interface {
	// Search answers packages matching query, ordered by how well, and how many match in
	// total so a caller can page through them.
	Search(ctx context.Context, query string, limit, offset int) (Results, error)

	// Recent answers the packages published to most recently, which is what a registry
	// with nothing searched for yet has to show.
	Recent(ctx context.Context, limit int) ([]Summary, error)

	// Detail answers everything a package's own page shows, or index.ErrNotFound.
	Detail(ctx context.Context, name pkgname.Name) (Detail, error)
}
