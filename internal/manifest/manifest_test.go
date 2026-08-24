package manifest

import (
	"strings"
	"testing"

	"github.com/rbx-loom/loom-pm/internal/index"
)

func parse(t *testing.T, content string) Manifest {
	t.Helper()

	parsed, err := Parse([]byte(content))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	return parsed
}

func rejects(t *testing.T, content, because string) {
	t.Helper()

	parsed, err := Parse([]byte(content))
	if err == nil {
		t.Fatalf("Parse returned %+v, want an error because it %s", parsed, because)
	}
}

func TestParseReadsAFullPackage(t *testing.T) {
	parsed := parse(t, `
project_type = "library"

[package]
name = "scope/serio"
version = "1.2.3-beta.1+build"
edition = "2026"
license = "Apache-2.0"
description = "Serialisation for Loom."
repository = "https://github.com/rbx-loom/serio"
authors = ["Ada", "Grace"]
realm = "server"

[files]
source_directory = "src"
`)

	if parsed.Package == nil {
		t.Fatal("no [package] was read")
	}

	if got := parsed.Package.Name.String(); got != "scope/serio" {
		t.Errorf("name = %q, want scope/serio", got)
	}

	if got := parsed.Package.Version.String(); got != "1.2.3-beta.1+build" {
		t.Errorf("version = %q, want it read whole", got)
	}

	if parsed.Package.Realm != index.RealmServer {
		t.Errorf("realm = %q, want server", parsed.Package.Realm)
	}

	if got := strings.Join(parsed.Package.Authors, ","); got != "Ada,Grace" {
		t.Errorf("authors = %q, want Ada,Grace", got)
	}

	if parsed.Package.License != "Apache-2.0" || parsed.Package.Edition != "2026" {
		t.Errorf("license/edition read as %q/%q", parsed.Package.License, parsed.Package.Edition)
	}
}

// A game is never published, so it has no [package] — that is not a parse failure, it is
// something publish refuses later with a diagnostic of its own.
func TestParseAllowsNoPackage(t *testing.T) {
	parsed := parse(t, `
project_type = "game"

[files]
source_directory = "src"
`)

	if parsed.Package != nil {
		t.Errorf("read a [package] that is not there: %+v", parsed.Package)
	}
}

func TestParseDefaultsRealmToShared(t *testing.T) {
	parsed := parse(t, `
[package]
name = "serio"
version = "1.0.0"
`)

	if parsed.Package.Realm != index.RealmShared {
		t.Errorf("realm = %q, want shared when the manifest names none", parsed.Package.Realm)
	}
}

// A dependency is written either as a bare requirement or as a table of one. A scoped
// name has to be quoted, because '/' is not allowed in a TOML bare key.
func TestParseReadsBothDependencyForms(t *testing.T) {
	parsed := parse(t, `
[package]
name = "serio"
version = "1.0.0"

[dependencies]
math = "^1.2"
runit = { version = "^0.4", dev = true }
"scope-of/thing" = ">=1.4, <2"
`)

	if len(parsed.Dependencies) != 3 {
		t.Fatalf("read %d dependencies, want 3", len(parsed.Dependencies))
	}

	byName := map[string]Dependency{}
	for _, dependency := range parsed.Dependencies {
		byName[dependency.Name.String()] = dependency
	}

	if got := byName["math"].Requirement.String(); got != "^1.2" {
		t.Errorf("math = %q, want the requirement as written", got)
	}

	if byName["math"].Dev {
		t.Error("math is marked dev")
	}

	if !byName["runit"].Dev {
		t.Error("runit is not marked dev")
	}

	if got := byName["runit"].Requirement.String(); got != "^0.4" {
		t.Errorf("runit = %q, want ^0.4", got)
	}
}

// Ordering a TOML table's keys is not something the parser promises, and publish writes
// these rows straight into the index.
func TestParseOrdersDependencies(t *testing.T) {
	content := `
[package]
name = "serio"
version = "1.0.0"

[dependencies]
zebra = "^1.0"
alpha = "^1.0"
"scope/aardvark" = "^1.0"
middle = "^1.0"
`

	want := []string{"alpha", "middle", "zebra", "scope/aardvark"}

	for range 5 {
		parsed := parse(t, content)
		if len(parsed.Dependencies) != len(want) {
			t.Fatalf("read %d dependencies, want %d", len(parsed.Dependencies), len(want))
		}

		for index, expected := range want {
			if got := parsed.Dependencies[index].Name.String(); got != expected {
				t.Fatalf("dependencies[%d] = %q, want %q", index, got, expected)
			}
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct{ name, content, because string }{
		{"malformed toml", `[package`, "is not TOML"},
		{
			"no name",
			"[package]\nversion = \"1.0.0\"\n",
			"has no name to publish under",
		},
		{
			"no version",
			"[package]\nname = \"serio\"\n",
			"has no version to publish under",
		},
		{
			"invalid name",
			"[package]\nname = \"1serio\"\nversion = \"1.0.0\"\n",
			"is not a package name",
		},
		{
			"invalid version",
			"[package]\nname = \"serio\"\nversion = \"1.0\"\n",
			"is not a version",
		},
		{
			"unknown realm",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\nrealm = \"everywhere\"\n",
			"is not a realm",
		},
		{
			"invalid dependency name",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\n\n[dependencies]\n\"1math\" = \"^1.0\"\n",
			"is not a package name",
		},
		{
			"invalid requirement",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\n\n[dependencies]\nmath = \"not a requirement\"\n",
			"is not a version requirement",
		},
		{
			"unsatisfiable requirement",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\n\n[dependencies]\nmath = \">=2, <1\"\n",
			"can never be satisfied",
		},
		{
			"dependency is neither string nor table",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\n\n[dependencies]\nmath = 12\n",
			"is not a requirement or a table",
		},
		{
			"dependency table has no version",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\n\n[dependencies]\nmath = { dev = true }\n",
			"names no version",
		},
		{
			"dependency version is not a string",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\n\n[dependencies]\nmath = { version = 1 }\n",
			"has a non-string version",
		},
		{
			"dev is not a boolean",
			"[package]\nname = \"serio\"\nversion = \"1.0.0\"\n\n[dependencies]\nmath = { version = \"^1.0\", dev = \"yes\" }\n",
			"has a non-boolean dev",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rejects(t, testCase.content, testCase.because)
		})
	}
}

// The registry derives what a package declares from the manifest inside the tarball, so a
// manifest naming the same dependency twice must not silently keep one of them.
func TestParseRejectsADuplicateDependency(t *testing.T) {
	rejects(t, `
[package]
name = "serio"
version = "1.0.0"

[dependencies]
math = "^1.0"
Math = "^2.0"
`, "names one dependency twice")
}
