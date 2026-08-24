// Package index assembles the sparse index document a client resolves from.
//
// The document answers what a package publishes without anyone fetching it, which is what
// IPackageIndex.Publications requires: one request per package, every version with its
// full dependency list.
package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

var ErrNotFound = errors.New("index: no such package")

// Realm governs which Rojo tree a package is vendored into.
type Realm string

const (
	RealmShared Realm = "shared"
	RealmClient Realm = "client"
	RealmServer Realm = "server"
)

func (r Realm) Valid() bool {
	switch r {
	case RealmShared, RealmClient, RealmServer:
		return true
	default:
		return false
	}
}

type Package struct {
	Name     pkgname.Name
	Versions []Version
}

type Version struct {
	Version      semver.Version
	Checksum     storage.Digest
	Yanked       bool
	Realm        Realm
	PublishedAt  time.Time
	Dependencies []Dependency
}

type Dependency struct {
	Name        pkgname.Name
	Requirement semver.Requirement
	Dev         bool
}

// Store is the index's view of the database.
type Store interface {
	// Package answers every version of name, or ErrNotFound. Order is Build's business.
	Package(ctx context.Context, name pkgname.Name) (Package, error)

	// Version answers one published version, or ErrNotFound. Yanked versions are still
	// answered: a lock file pinning one must keep installing.
	Version(ctx context.Context, name pkgname.Name, version semver.Version) (Version, error)
}

// Document is a rendered index response and the ETag it revalidates with.
type Document struct {
	Body []byte
	ETag string
}

// Build renders pkg, ordering versions ascending and dependencies by name.
//
// Both orderings are enforced here rather than trusted from the store: ascending versions
// are what LockResolver.Choose depends on, and stable dependencies are what keep the ETag
// from flapping between two queries that returned the same rows in a different order.
func Build(pkg Package) (Document, error) {
	versions := slices.Clone(pkg.Versions)
	slices.SortFunc(versions, func(left, right Version) int {
		return left.Version.Compare(right.Version)
	})

	rendered := document{
		Name:     pkg.Name.String(),
		Versions: make([]versionDocument, 0, len(versions)),
	}

	for _, version := range versions {
		dependencies := slices.Clone(version.Dependencies)
		slices.SortFunc(dependencies, func(left, right Dependency) int {
			return left.Name.Compare(right.Name)
		})

		built := versionDocument{
			Version:      version.Version.String(),
			Checksum:     version.Checksum.String(),
			Yanked:       version.Yanked,
			Realm:        string(version.Realm),
			PublishedAt:  version.PublishedAt.UTC(),
			Dependencies: make([]dependencyDocument, 0, len(dependencies)),
		}

		for _, dependency := range dependencies {
			built.Dependencies = append(built.Dependencies, dependencyDocument{
				Name:        dependency.Name.String(),
				Requirement: dependency.Requirement.String(),
				Dev:         dependency.Dev,
			})
		}

		rendered.Versions = append(rendered.Versions, built)
	}

	body, err := json.Marshal(rendered)
	if err != nil {
		return Document{}, fmt.Errorf("index: rendering %s: %w", pkg.Name, err)
	}

	// hashing the body rather than a row timestamp: an ETag derived from metadata is
	// stale exactly when someone forgets to bump that metadata, and the body is small
	sum := sha256.Sum256(body)
	return Document{Body: body, ETag: `"` + hex.EncodeToString(sum[:]) + `"`}, nil
}

type document struct {
	Name     string            `json:"name"`
	Versions []versionDocument `json:"versions"`
}

type versionDocument struct {
	Version      string               `json:"version"`
	Checksum     string               `json:"checksum"`
	Yanked       bool                 `json:"yanked"`
	Realm        string               `json:"realm"`
	PublishedAt  time.Time            `json:"published_at"`
	Dependencies []dependencyDocument `json:"dependencies"`
}

type dependencyDocument struct {
	Name        string `json:"name"`
	Requirement string `json:"requirement"`
	Dev         bool   `json:"dev"`
}
