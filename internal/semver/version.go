package semver

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// MaxComponent bounds a version component to what the C# original holds. Loom parses
// components into an int32 and treats overflow as an invalid version, so an unbounded
// Go int would accept versions the client rejects.
const MaxComponent = 1<<31 - 1

type Version struct {
	Major         int
	Minor         int
	Patch         int
	Prerelease    string
	BuildMetadata string
}

// ParseVersion reads major.minor.patch[-prerelease][+build]. Build metadata is split off
// before the prerelease, so a prerelease may not contain '+' but metadata may contain '-'.
func ParseVersion(text string) (Version, error) {
	if strings.TrimSpace(text) == "" {
		return Version{}, fmt.Errorf("version cannot be empty")
	}

	remainder := text

	var buildMetadata string
	if index := strings.IndexByte(remainder, '+'); index >= 0 {
		buildMetadata, remainder = remainder[index+1:], remainder[:index]
		if err := validateIdentifiers(buildMetadata, "build metadata", false); err != nil {
			return Version{}, err
		}
	}

	var prerelease string
	if index := strings.IndexByte(remainder, '-'); index >= 0 {
		prerelease, remainder = remainder[index+1:], remainder[:index]
		if err := validateIdentifiers(prerelease, "pre-release", true); err != nil {
			return Version{}, err
		}
	}

	components := strings.Split(remainder, ".")
	if len(components) != 3 {
		return Version{}, fmt.Errorf("version %q must have exactly three components, written 'major.minor.patch'", text)
	}

	var numbers [3]int
	for index, component := range components {
		number, ok := parseNumericIdentifier(component)
		if !ok {
			return Version{}, fmt.Errorf("version %q has an invalid %s component %q", text, componentNames[index], component)
		}
		numbers[index] = number
	}

	return Version{
		Major:         numbers[0],
		Minor:         numbers[1],
		Patch:         numbers[2],
		Prerelease:    prerelease,
		BuildMetadata: buildMetadata,
	}, nil
}

var componentNames = [3]string{"major", "minor", "patch"}

func (v Version) IsPrerelease() bool {
	return v.Prerelease != ""
}

// Compare orders by semver precedence. Build metadata takes no part in it.
func (v Version) Compare(other Version) int {
	if ordering := cmp.Compare(v.Major, other.Major); ordering != 0 {
		return ordering
	}
	if ordering := cmp.Compare(v.Minor, other.Minor); ordering != 0 {
		return ordering
	}
	if ordering := cmp.Compare(v.Patch, other.Patch); ordering != 0 {
		return ordering
	}
	return comparePrerelease(v.Prerelease, other.Prerelease)
}

func (v Version) Equal(other Version) bool {
	return v.Compare(other) == 0
}

func (v Version) String() string {
	version := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Prerelease != "" {
		version += "-" + v.Prerelease
	}
	if v.BuildMetadata != "" {
		version += "+" + v.BuildMetadata
	}
	return version
}

// SortVersions orders ascending, newest last. LockResolver.Choose takes the last
// satisfying entry rather than the maximum, so an index served in any other order makes
// resolution pick an older version without reporting anything.
func SortVersions(versions []Version) {
	slices.SortFunc(versions, Version.Compare)
}

func comparePrerelease(left, right string) int {
	if left == "" || right == "" {
		switch {
		case left == right:
			return 0
		case left == "":
			return 1
		default:
			return -1
		}
	}

	leftIdentifiers, rightIdentifiers := strings.Split(left, "."), strings.Split(right, ".")
	for index := range min(len(leftIdentifiers), len(rightIdentifiers)) {
		if ordering := compareIdentifier(leftIdentifiers[index], rightIdentifiers[index]); ordering != 0 {
			return ordering
		}
	}

	return cmp.Compare(len(leftIdentifiers), len(rightIdentifiers))
}

func compareIdentifier(left, right string) int {
	leftNumber, leftIsNumeric := parseNumericIdentifier(left)
	rightNumber, rightIsNumeric := parseNumericIdentifier(right)

	switch {
	case leftIsNumeric && rightIsNumeric:
		return cmp.Compare(leftNumber, rightNumber)
	case leftIsNumeric:
		return -1
	case rightIsNumeric:
		return 1
	default:
		return strings.Compare(left, right)
	}
}

// validateIdentifiers checks the dot-separated identifiers of a prerelease or of build
// metadata. Only a prerelease rejects leading zeroes, so "1.0.0+01" is a valid version
// and "1.0.0-01" is not.
func validateIdentifiers(text, kind string, rejectLeadingZeroes bool) error {
	if text == "" {
		return fmt.Errorf("%s identifiers cannot be empty", kind)
	}

	for _, identifier := range strings.Split(text, ".") {
		if identifier == "" {
			return fmt.Errorf("%s identifiers cannot be empty", kind)
		}

		isNumeric := true
		for index := range len(identifier) {
			character := identifier[index]
			switch {
			case character >= '0' && character <= '9':
			case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character == '-':
				isNumeric = false
			default:
				return fmt.Errorf("%s identifier %q may only contain letters, digits and '-'", kind, identifier)
			}
		}

		if rejectLeadingZeroes && isNumeric && len(identifier) > 1 && identifier[0] == '0' {
			return fmt.Errorf("%s identifier %q cannot have leading zeroes", kind, identifier)
		}
	}

	return nil
}

// parseNumericIdentifier reads an identifier that must be ASCII digits with no leading
// zero and within MaxComponent. Overflow answers false rather than an error, which is
// what makes an oversized prerelease identifier compare as alphanumeric.
func parseNumericIdentifier(text string) (int, bool) {
	if text == "" || (len(text) > 1 && text[0] == '0') {
		return 0, false
	}

	value := 0
	for index := range len(text) {
		character := text[index]
		if character < '0' || character > '9' {
			return 0, false
		}

		value = value*10 + int(character-'0')
		if value > MaxComponent {
			return 0, false
		}
	}

	return value, true
}
