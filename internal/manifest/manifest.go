// Package manifest reads loom-config.toml, the same file Loom.Config reads.
//
// Only the tables the registry derives a publication from: [package] and [dependencies].
package manifest

import (
	"fmt"
	"slices"

	"github.com/BurntSushi/toml"
	"github.com/rbx-loom/loom-pm/internal/index"
	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
)

const FileName = "loom-config.toml"

// MaxDependencies bounds what one version may declare. Every dependency costs the registry
// a resolvability check at publish time, so an unbounded list turns one small upload into
// as much database work as the author cares to ask for.
const MaxDependencies = 256

type Manifest struct {
	// Package is nil for a project that is never published, such as a game.
	Package      *Package
	Dependencies []Dependency
}

type Package struct {
	Name        pkgname.Name
	Version     semver.Version
	Edition     string
	License     string
	Description string
	Repository  string
	Authors     []string
	Realm       index.Realm
}

type Dependency struct {
	Name        pkgname.Name
	Requirement semver.Requirement
	Dev         bool
}

func Parse(content []byte) (Manifest, error) {
	var raw struct {
		Package *struct {
			Name        string   `toml:"name"`
			Version     string   `toml:"version"`
			Edition     string   `toml:"edition"`
			License     string   `toml:"license"`
			Description string   `toml:"description"`
			Repository  string   `toml:"repository"`
			Authors     []string `toml:"authors"`
			Realm       string   `toml:"realm"`
		} `toml:"package"`

		Dependencies map[string]any `toml:"dependencies"`
	}

	if err := toml.Unmarshal(content, &raw); err != nil {
		return Manifest{}, fmt.Errorf("%s is not valid TOML: %w", FileName, err)
	}

	parsed := Manifest{}

	if raw.Package != nil {
		name, err := pkgname.Parse(raw.Package.Name)
		if err != nil {
			return Manifest{}, fmt.Errorf("[package] name: %w", err)
		}

		version, err := semver.ParseVersion(raw.Package.Version)
		if err != nil {
			return Manifest{}, fmt.Errorf("[package] version: %w", err)
		}

		realm := index.RealmShared
		if raw.Package.Realm != "" {
			realm = index.Realm(raw.Package.Realm)
			if !realm.Valid() {
				return Manifest{}, fmt.Errorf("unknown realm %q; expected 'shared', 'client' or 'server'", raw.Package.Realm)
			}
		}

		// never nil: authors reaches a TEXT[] NOT NULL column, where a nil slice is NULL
		authors := raw.Package.Authors
		if authors == nil {
			authors = []string{}
		}

		parsed.Package = &Package{
			Name:        name,
			Version:     version,
			Edition:     raw.Package.Edition,
			License:     raw.Package.License,
			Description: raw.Package.Description,
			Repository:  raw.Package.Repository,
			Authors:     authors,
			Realm:       realm,
		}
	}

	dependencies, err := readDependencies(raw.Dependencies)
	if err != nil {
		return Manifest{}, err
	}

	parsed.Dependencies = dependencies
	return parsed, nil
}

func readDependencies(entries map[string]any) ([]Dependency, error) {
	if len(entries) > MaxDependencies {
		return nil, fmt.Errorf("[dependencies] names %d packages, and at most %d may be declared",
			len(entries), MaxDependencies)
	}

	dependencies := make([]Dependency, 0, len(entries))
	seen := make(map[string]string, len(entries))

	for specifier, entry := range entries {
		name, err := pkgname.Parse(specifier)
		if err != nil {
			return nil, fmt.Errorf("[dependencies] %q: %w", specifier, err)
		}

		// names are compared case-insensitively, so two spellings of one name are one
		// dependency written twice rather than two
		if previous, ok := seen[name.Normalized()]; ok {
			return nil, fmt.Errorf("[dependencies] names %q more than once, as %q and %q", name, previous, specifier)
		}
		seen[name.Normalized()] = specifier

		dependency, err := readDependency(name, entry)
		if err != nil {
			return nil, err
		}

		dependencies = append(dependencies, dependency)
	}

	slices.SortFunc(dependencies, func(left, right Dependency) int {
		return left.Name.Compare(right.Name)
	})

	return dependencies, nil
}

func readDependency(name pkgname.Name, entry any) (Dependency, error) {
	switch written := entry.(type) {
	case string:
		requirement, err := semver.ParseRequirement(written)
		if err != nil {
			return Dependency{}, fmt.Errorf("[dependencies] %q: %w", name, err)
		}

		return Dependency{Name: name, Requirement: requirement}, nil

	case map[string]any:
		text, ok := written["version"]
		if !ok {
			return Dependency{}, fmt.Errorf("[dependencies] %q names no version; write it as { version = \"^1.2\" }", name)
		}

		statement, ok := text.(string)
		if !ok {
			return Dependency{}, fmt.Errorf("[dependencies] %q must have a version requirement written as a string, e.g. \"^1.2\"", name)
		}

		requirement, err := semver.ParseRequirement(statement)
		if err != nil {
			return Dependency{}, fmt.Errorf("[dependencies] %q: %w", name, err)
		}

		dependency := Dependency{Name: name, Requirement: requirement}
		if development, ok := written["dev"]; ok {
			dependency.Dev, ok = development.(bool)
			if !ok {
				return Dependency{}, fmt.Errorf("[dependencies] %q has a 'dev' that is not true or false", name)
			}
		}

		return dependency, nil

	default:
		return Dependency{}, fmt.Errorf("[dependencies] %q must be a version requirement string or a table with a 'version' key", name)
	}
}
