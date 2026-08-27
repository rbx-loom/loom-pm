// Package pkgname ports Loom.Config.PackageName: the identity of a Loom package, written
// "scope/name" or "name", compared case-insensitively because it doubles as a path
// segment on disk.
//
// testdata/conformance/package-name.json is shared with rbx-loom/loom. Normalisation and
// squat keys are registry concepts with no C# counterpart.
package pkgname

import (
	"cmp"
	"fmt"
	"strings"
)

const MaxSegmentLength = 64

// Name is always valid: the zero value aside, one can only be obtained from Parse.
type Name struct {
	scope string
	name  string
}

func Parse(text string) (Name, error) {
	if strings.TrimSpace(text) == "" {
		return Name{}, fmt.Errorf("package name cannot be empty")
	}

	scope, name, scoped := strings.Cut(text, "/")
	if !scoped {
		scope, name = "", text
	}

	if strings.Contains(name, "/") {
		return Name{}, fmt.Errorf("package name %q may contain at most one '/', separating the scope from the name", text)
	}

	if scoped {
		if err := validateSegment(scope, "scope"); err != nil {
			return Name{}, err
		}
	}

	if err := validateSegment(name, "name"); err != nil {
		return Name{}, err
	}

	return Name{scope: scope, name: name}, nil
}

// ValidateScope checks a scope on its own, for the places one is named without a package
// after it.
func ValidateScope(text string) error {
	return validateSegment(strings.TrimSpace(text), "scope")
}

func (n Name) Scope() string { return n.scope }
func (n Name) Name() string  { return n.name }

func (n Name) IsScoped() bool { return n.scope != "" }

func (n Name) String() string {
	if n.scope == "" {
		return n.name
	}

	return n.scope + "/" + n.name
}

// Compare orders names the way they are compared, so a file listing packages is written
// in one order whoever writes it. Every unscoped name sorts before every scoped one.
func (n Name) Compare(other Name) int {
	if ordering := cmp.Compare(fold(n.scope), fold(other.scope)); ordering != 0 {
		return ordering
	}

	return cmp.Compare(fold(n.name), fold(other.name))
}

func (n Name) Equal(other Name) bool {
	return n.Compare(other) == 0
}

// Normalized is the lookup key: the name lowercased, scope included.
func (n Name) Normalized() string {
	return fold(n.String())
}

// NormalizedScope is the scope lowercased, empty for an unscoped name.
func (n Name) NormalizedScope() string {
	return fold(n.scope)
}

// SquatKey collapses the characters a name may contain that read alike, so "my-thing" and
// "my_thing" cannot be registered by two different people. It gates creation only; lookup
// is always by Normalized.
func (n Name) SquatKey() string {
	return strings.ReplaceAll(n.Normalized(), "_", "-")
}

func fold(text string) string {
	return strings.ToLower(text)
}

func validateSegment(segment, kind string) error {
	switch {
	case segment == "":
		return fmt.Errorf("package %s cannot be empty", kind)
	case len(segment) > MaxSegmentLength:
		return fmt.Errorf("package %s %q may be at most %d characters", kind, segment, MaxSegmentLength)
	case !isAsciiLetter(segment[0]):
		return fmt.Errorf("package %s %q must start with a letter", kind, segment)
	}

	for index := range len(segment) {
		character := segment[index]
		if isAsciiLetter(character) || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}

		return fmt.Errorf("package %s %q may only contain letters, digits, '-' and '_'", kind, segment)
	}

	return nil
}

func isAsciiLetter(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
}
