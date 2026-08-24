package semver

import (
	"fmt"
	"strings"
)

// Bound is one end of the interval a requirement accepts, and whether the version naming
// it is itself accepted.
type Bound struct {
	Version   Version
	Inclusive bool
}

// Requirement is a version requirement as written in a manifest ("^1.2", ">=1.4, <2"),
// read as the single interval it accepts. Clause forms all name intervals and
// comma-separated clauses intersect, so there is deliberately no union here: if this
// needs a slice of clauses, the port has drifted from Loom.Config.VersionRequirement.
type Requirement struct {
	text  string
	Lower *Bound
	Upper *Bound
}

// ParseRequirement reads a comma-separated clause list. A requirement no version can
// satisfy (">=2, <1") is an error rather than an empty interval, so a Requirement that
// exists always names at least one version.
func ParseRequirement(text string) (Requirement, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Requirement{}, fmt.Errorf("version requirement cannot be empty")
	}

	var lower, upper *Bound
	for _, clause := range strings.Split(trimmed, ",") {
		clauseLower, clauseUpper, err := parseClause(clause)
		if err != nil {
			return Requirement{}, err
		}

		lower, upper = tighterLower(lower, clauseLower), tighterUpper(upper, clauseUpper)
	}

	if isEmpty(lower, upper) {
		return Requirement{}, fmt.Errorf("version requirement %q cannot be satisfied by any version", trimmed)
	}

	return Requirement{text: trimmed, Lower: lower, Upper: upper}, nil
}

func Any() Requirement {
	return Requirement{text: "*"}
}

func (r Requirement) IsAny() bool {
	return r.Lower == nil && r.Upper == nil
}

// Satisfies reports whether version is accepted. A prerelease clears the interval check
// and is then still rejected unless one of the requirement's own bounds names a
// prerelease of the same major.minor.patch: ">=1.2.0" asks for released versions, and
// 1.3.0-beta.1 is not one.
func (r Requirement) Satisfies(version Version) bool {
	if r.Lower != nil {
		ordering := version.Compare(r.Lower.Version)
		if ordering < 0 || (ordering == 0 && !r.Lower.Inclusive) {
			return false
		}
	}

	if r.Upper != nil {
		ordering := version.Compare(r.Upper.Version)
		if ordering > 0 || (ordering == 0 && !r.Upper.Inclusive) {
			return false
		}
	}

	return !version.IsPrerelease() || namesPrereleaseOf(r.Lower, version) || namesPrereleaseOf(r.Upper, version)
}

// Intersect returns the requirement accepting exactly the versions both accept, and false
// when they accept none in common.
func (r Requirement) Intersect(other Requirement) (Requirement, bool) {
	lower, upper := tighterLower(r.Lower, other.Lower), tighterUpper(r.Upper, other.Upper)

	// keeping an operand whole preserves the text it was written as, which is what a
	// diagnostic quotes back
	switch {
	case sameBound(lower, r.Lower) && sameBound(upper, r.Upper):
		return r, true
	case sameBound(lower, other.Lower) && sameBound(upper, other.Upper):
		return other, true
	case isEmpty(lower, upper):
		return Requirement{}, false
	}

	return Requirement{text: describe(lower, upper), Lower: lower, Upper: upper}, true
}

// IntersectAll intersects every requirement asking for one package. An empty slice
// constrains nothing and answers Any.
func IntersectAll(requirements []Requirement) (Requirement, bool) {
	if len(requirements) == 0 {
		return Any(), true
	}

	result := requirements[0]
	for _, requirement := range requirements[1:] {
		narrowed, ok := result.Intersect(requirement)
		if !ok {
			return Requirement{}, false
		}

		result = narrowed
	}

	return result, true
}

// String returns the requirement as written, which is the form a manifest author
// recognises in a diagnostic. A Requirement built by hand rather than parsed has no
// written form and falls back to its interval.
func (r Requirement) String() string {
	if r.text == "" {
		return r.ComparatorString()
	}

	return r.text
}

// ComparatorString renders the interval, whatever the requirement was written as.
func (r Requirement) ComparatorString() string {
	return describe(r.Lower, r.Upper)
}

// partial is a version as a clause wrote it. The count of components actually written
// decides where a '^', '~' or partial '=' ceiling sits, so padding cannot discard it.
type partial struct {
	completed Version
	written   int
}

var comparators = []string{">=", "<=", ">", "<", "^", "~", "="}

func parseClause(clause string) (lower, upper *Bound, err error) {
	trimmed := strings.TrimSpace(clause)
	switch trimmed {
	case "":
		return nil, nil, fmt.Errorf("a version requirement clause cannot be empty")
	case "*":
		return nil, nil, nil
	}

	comparator := ""
	for _, candidate := range comparators {
		if strings.HasPrefix(trimmed, candidate) {
			comparator = candidate
			break
		}
	}

	written, err := parsePartial(strings.TrimSpace(trimmed[len(comparator):]))
	if err != nil {
		return nil, nil, err
	}

	version := written.completed
	switch comparator {
	case ">=":
		return &Bound{version, true}, nil, nil
	case ">":
		return &Bound{version, false}, nil, nil
	case "<=":
		return nil, &Bound{version, true}, nil
	case "<":
		return nil, &Bound{version, false}, nil
	case "~":
		return &Bound{version, true}, &Bound{written.writtenCeiling(), false}, nil
	case "=":
		if written.written == 3 {
			return &Bound{version, true}, &Bound{version, true}, nil
		}
		return &Bound{version, true}, &Bound{written.writtenCeiling(), false}, nil
	default:
		// a bare clause means caret
		return &Bound{version, true}, &Bound{written.caretCeiling(), false}, nil
	}
}

func parsePartial(text string) (partial, error) {
	if text == "" {
		return partial{}, fmt.Errorf("a version requirement clause must name a version")
	}

	if strings.ContainsRune(text, '+') {
		return partial{}, fmt.Errorf("version requirement %q cannot name build metadata, which takes no part in comparison", text)
	}

	numeric, prerelease := text, ""
	hasPrerelease := false
	if index := strings.IndexByte(text, '-'); index >= 0 {
		numeric, prerelease, hasPrerelease = text[:index], text[index+1:], true
	}

	components := strings.Split(numeric, ".")
	if len(components) > 3 {
		return partial{}, fmt.Errorf("version requirement %q may name at most three components, written 'major.minor.patch'", text)
	}

	padded := []string{"0", "0", "0"}
	copy(padded, components)

	completed := strings.Join(padded, ".")
	if hasPrerelease {
		completed += "-" + prerelease
	}

	version, err := ParseVersion(completed)
	if err != nil {
		return partial{}, err
	}

	return partial{completed: version, written: len(components)}, nil
}

// caretCeiling is where '^' stops: the leftmost written component that is non-zero,
// incremented. A zero major or minor makes the next component the compatibility
// boundary, and when everything written is zero the last written component moves. The
// cases are ordered, not independent.
//
//	^1.2.3 -> <2.0.0    ^0 -> <1.0.0    ^0.2.3 -> <0.3.0    ^0.0 -> <0.1.0    ^0.0.3 -> <0.0.4
func (p partial) caretCeiling() Version {
	switch {
	case p.completed.Major != 0:
		return Version{Major: p.completed.Major + 1}
	case p.written == 1:
		return Version{Major: 1}
	case p.completed.Minor != 0:
		return Version{Minor: p.completed.Minor + 1}
	case p.written == 2:
		return Version{Minor: 1}
	default:
		return Version{Patch: p.completed.Patch + 1}
	}
}

// writtenCeiling is past everything the unwritten components could have been: the last
// written component incremented. Three written components still move the minor, which is
// what makes ~1.2.3 mean [1.2.3, 1.3.0).
func (p partial) writtenCeiling() Version {
	if p.written == 1 {
		return Version{Major: p.completed.Major + 1}
	}

	return Version{Major: p.completed.Major, Minor: p.completed.Minor + 1}
}

func tighterLower(left, right *Bound) *Bound {
	switch {
	case left == nil:
		return right
	case right == nil:
		return left
	}

	if ordering := left.Version.Compare(right.Version); ordering != 0 {
		if ordering > 0 {
			return left
		}
		return right
	}

	// the same version bounds more tightly when it is excluded
	if left.Inclusive {
		return right
	}
	return left
}

func tighterUpper(left, right *Bound) *Bound {
	switch {
	case left == nil:
		return right
	case right == nil:
		return left
	}

	if ordering := left.Version.Compare(right.Version); ordering != 0 {
		if ordering < 0 {
			return left
		}
		return right
	}

	if left.Inclusive {
		return right
	}
	return left
}

func isEmpty(lower, upper *Bound) bool {
	if lower == nil || upper == nil {
		return false
	}

	ordering := lower.Version.Compare(upper.Version)
	return ordering > 0 || (ordering == 0 && !(lower.Inclusive && upper.Inclusive))
}

func namesPrereleaseOf(bound *Bound, version Version) bool {
	return bound != nil &&
		bound.Version.IsPrerelease() &&
		bound.Version.Major == version.Major &&
		bound.Version.Minor == version.Minor &&
		bound.Version.Patch == version.Patch
}

// sameBound compares by value, not by pointer, so an operand rebuilt elsewhere still
// counts as unchanged.
func sameBound(left, right *Bound) bool {
	if left == nil || right == nil {
		return left == right
	}

	return *left == *right
}

func describe(lower, upper *Bound) string {
	if lower == nil {
		if upper == nil {
			return "*"
		}
		return upperOperator(upper) + upper.Version.String()
	}

	floor := lowerOperator(lower) + lower.Version.String()
	if upper == nil {
		return floor
	}

	if lower.Inclusive && upper.Inclusive && lower.Version.Equal(upper.Version) {
		return "=" + lower.Version.String()
	}

	return floor + ", " + upperOperator(upper) + upper.Version.String()
}

func lowerOperator(bound *Bound) string {
	if bound.Inclusive {
		return ">="
	}
	return ">"
}

func upperOperator(bound *Bound) string {
	if bound.Inclusive {
		return "<="
	}
	return "<"
}
