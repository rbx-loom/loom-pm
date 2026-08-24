package index

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rbx-loom/loom-pm/internal/pkgname"
	"github.com/rbx-loom/loom-pm/internal/semver"
	"github.com/rbx-loom/loom-pm/internal/storage"
)

func mustName(t *testing.T, text string) pkgname.Name {
	t.Helper()

	name, err := pkgname.Parse(text)
	if err != nil {
		t.Fatalf("pkgname.Parse(%q): %v", text, err)
	}

	return name
}

func mustVersion(t *testing.T, text string) semver.Version {
	t.Helper()

	version, err := semver.ParseVersion(text)
	if err != nil {
		t.Fatalf("semver.ParseVersion(%q): %v", text, err)
	}

	return version
}

func mustRequirement(t *testing.T, text string) semver.Requirement {
	t.Helper()

	requirement, err := semver.ParseRequirement(text)
	if err != nil {
		t.Fatalf("semver.ParseRequirement(%q): %v", text, err)
	}

	return requirement
}

func versionOf(t *testing.T, text string, dependencies ...Dependency) Version {
	t.Helper()

	return Version{
		Version:      mustVersion(t, text),
		Checksum:     storage.DigestOf([]byte(text)),
		Realm:        RealmShared,
		PublishedAt:  time.Date(2026, 3, 14, 9, 21, 0, 0, time.UTC),
		Dependencies: dependencies,
	}
}

func decode(t *testing.T, document Document) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(document.Body, &decoded); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}

	return decoded
}

// LockResolver.Choose takes the LAST satisfying entry rather than the maximum, so a
// document served in any other order makes resolution pick an older version without
// reporting anything. Build sorts so that no caller can get this wrong.
func TestBuildOrdersVersionsAscending(t *testing.T) {
	document, err := Build(Package{
		Name: mustName(t, "serio"),
		Versions: []Version{
			versionOf(t, "1.10.0"),
			versionOf(t, "1.2.0"),
			versionOf(t, "1.0.0-rc.1"),
			versionOf(t, "1.9.0"),
			versionOf(t, "1.0.0"),
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{"1.0.0-rc.1", "1.0.0", "1.2.0", "1.9.0", "1.10.0"}

	versions := decode(t, document)["versions"].([]any)
	if len(versions) != len(want) {
		t.Fatalf("built %d versions, want %d", len(versions), len(want))
	}

	for index, expected := range want {
		got := versions[index].(map[string]any)["version"]
		if got != expected {
			t.Errorf("versions[%d] = %v, want %q", index, got, expected)
		}
	}
}

// The ETag is over the body, so a dependency order that varied between queries would make
// it flap and defeat revalidation entirely.
func TestBuildOrdersDependenciesByName(t *testing.T) {
	document, err := Build(Package{
		Name: mustName(t, "serio"),
		Versions: []Version{
			versionOf(t, "1.0.0",
				Dependency{Name: mustName(t, "zebra"), Requirement: mustRequirement(t, "^1.0")},
				Dependency{Name: mustName(t, "scope/alpha"), Requirement: mustRequirement(t, "^2.0")},
				Dependency{Name: mustName(t, "alpha"), Requirement: mustRequirement(t, "^3.0")},
			),
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	want := []string{"alpha", "zebra", "scope/alpha"}

	versions := decode(t, document)["versions"].([]any)
	dependencies := versions[0].(map[string]any)["dependencies"].([]any)
	if len(dependencies) != len(want) {
		t.Fatalf("built %d dependencies, want %d", len(dependencies), len(want))
	}

	for index, expected := range want {
		got := dependencies[index].(map[string]any)["name"]
		if got != expected {
			t.Errorf("dependencies[%d] = %v, want %q", index, got, expected)
		}
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	build := func() Document {
		document, err := Build(Package{
			Name: mustName(t, "serio"),
			Versions: []Version{
				versionOf(t, "1.2.0", Dependency{Name: mustName(t, "math"), Requirement: mustRequirement(t, "^1.0")}),
				versionOf(t, "1.0.0"),
			},
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return document
	}

	first, second := build(), build()
	if string(first.Body) != string(second.Body) {
		t.Error("Build is not byte-stable for equal input")
	}

	if first.ETag != second.ETag {
		t.Errorf("ETag = %q then %q for equal input", first.ETag, second.ETag)
	}
}

func TestETagChangesWithContent(t *testing.T) {
	one, err := Build(Package{Name: mustName(t, "serio"), Versions: []Version{versionOf(t, "1.0.0")}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	two, err := Build(Package{
		Name:     mustName(t, "serio"),
		Versions: []Version{versionOf(t, "1.0.0"), versionOf(t, "1.1.0")},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if one.ETag == two.ETag {
		t.Errorf("ETag is %q for both a one-version and a two-version package", one.ETag)
	}
}

// A yanked version stays in the document: an existing lock file pinning it must still
// install. Excluding it from a NEW resolution is the client's job, which it can only do
// if the flag reaches it.
func TestBuildIncludesYankedVersions(t *testing.T) {
	yanked := versionOf(t, "1.0.0")
	yanked.Yanked = true

	document, err := Build(Package{Name: mustName(t, "serio"), Versions: []Version{yanked}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	versions := decode(t, document)["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("built %d versions, want the yanked one", len(versions))
	}

	if versions[0].(map[string]any)["yanked"] != true {
		t.Error("the yanked version is not marked yanked")
	}
}

func TestBuildIncludesDevDependencies(t *testing.T) {
	document, err := Build(Package{
		Name: mustName(t, "serio"),
		Versions: []Version{
			versionOf(t, "1.0.0", Dependency{
				Name:        mustName(t, "runit"),
				Requirement: mustRequirement(t, "^0.4"),
				Dev:         true,
			}),
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	versions := decode(t, document)["versions"].([]any)
	dependencies := versions[0].(map[string]any)["dependencies"].([]any)
	if len(dependencies) != 1 {
		t.Fatalf("built %d dependencies, want the dev one", len(dependencies))
	}

	if dependencies[0].(map[string]any)["dev"] != true {
		t.Error("the dev dependency is not marked dev")
	}
}

func TestBuildRendersChecksumAndRequirement(t *testing.T) {
	version := versionOf(t, "1.0.0", Dependency{Name: mustName(t, "math"), Requirement: mustRequirement(t, "^1.2")})

	document, err := Build(Package{Name: mustName(t, "serio"), Versions: []Version{version}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	built := decode(t, document)["versions"].([]any)[0].(map[string]any)

	if got := built["checksum"]; got != version.Checksum.String() {
		t.Errorf("checksum = %v, want %q", got, version.Checksum.String())
	}

	if got := built["realm"]; got != string(RealmShared) {
		t.Errorf("realm = %v, want %q", got, RealmShared)
	}

	requirement := built["dependencies"].([]any)[0].(map[string]any)["requirement"]
	if requirement != "^1.2" {
		t.Errorf("requirement = %v, want the form it was written in", requirement)
	}
}

// A nil slice marshals to null, which is not an empty list to a client decoding it.
func TestBuildRendersEmptyListsAsArrays(t *testing.T) {
	document, err := Build(Package{
		Name:     mustName(t, "serio"),
		Versions: []Version{versionOf(t, "1.0.0")},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	body := string(document.Body)
	if got := decode(t, document)["versions"].([]any)[0].(map[string]any)["dependencies"]; got == nil {
		t.Errorf("dependencies rendered as null in %s", body)
	}

	empty, err := Build(Package{Name: mustName(t, "serio")})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if decode(t, empty)["versions"] == nil {
		t.Errorf("versions rendered as null in %s", empty.Body)
	}
}
